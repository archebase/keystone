#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Fit one fixed right-to-left photometric correction from stereo frame pairs.

This module owns the whole colour-consistency algorithm: matching the two eyes,
fitting a luminance-binned gain curve per channel plus a smooth spatial gain
field, validating the fitted model on held-out blocks of the recording, and
deciding whether the correction is worth applying at all. Callers only feed
decoded frame pairs and then apply the returned plan, so MCAP, H.264, topics,
and job orchestration stay outside this seam.

The correction always maps the *source* eye (right) onto the *reference* eye
(left). It is photometric only: it cannot remove lens-internal chromatic
aberration, and with no colour chart it can only claim consistency with the
reference eye, never absolute colorimetric accuracy.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass

import cv2
import numpy as np

#: Decision values reported by :meth:`ColorConsistencyCalibrator.fit`.
DECISION_APPLIED = "applied"
DECISION_NOT_NEEDED = "not_needed"
DECISION_INSUFFICIENT = "insufficient"
DECISION_REJECTED = "rejected"

#: Algorithm identity recorded in the manifest so an output stays explainable.
ALGORITHM_VERSION = "stereo-color-v1"

#: Size of the linear-light grid used to invert the transfer function.
_LINEAR_GRID_SIZE = 65536

#: Source level above which a sample counts as clipped when measuring how much
#: headroom a correction consumes.
UNSATURATED_LEVEL = 250

_XYZ_FROM_SRGB = np.array([
    [.4124564, .3575761, .1804375],
    [.2126729, .7151522, .0721750],
    [.0193339, .1191920, .9503041],
])
_WHITE_XYZ = np.array([.95047, 1., 1.08883])


def _build_transfer_tables() -> tuple[np.ndarray, np.ndarray]:
    """Return the sRGB8->linear table and the linear->sRGB8 grid."""
    encoded = np.arange(256, dtype=np.float64) / 255.0
    srgb_to_linear = np.where(
        encoded <= .04045,
        encoded / 12.92,
        ((encoded + .055) / 1.055) ** 2.4,
    ).astype(np.float32)
    grid = np.linspace(0.0, 1.0, _LINEAR_GRID_SIZE, dtype=np.float32)
    linear_to_srgb = np.rint(np.where(
        grid <= .0031308,
        12.92 * grid,
        1.055 * grid ** (1 / 2.4) - .055,
    ) * 255.0)
    return srgb_to_linear, np.clip(linear_to_srgb, 0, 255).astype(np.uint8)


_SRGB8_TO_LINEAR, _LINEAR_TO_SRGB8 = _build_transfer_tables()


@dataclass(frozen=True)
class ColorConsistencyConfig:
    """Fixed algorithm parameters; a stereo-split image pins them all.

    The defaults are the configuration measured on real DECXIN recordings. They
    are deliberately not runtime settings: one image digest must describe one
    deterministic processing behaviour so a release can be rolled back by
    selecting the previous digest.
    """

    #: Decode/analyse every Nth stereo frame.
    sample_step: int = 20
    #: Scale used for feature matching; 0.5 is materially faster than 1.0.
    match_scale: float = .5
    #: Feature budget per sampled frame and per eye.
    max_match_features: int = 1000
    #: Hard cap on retained correspondences, keeping memory bounded.
    max_samples: int = 100_000
    #: Per-channel gain curve resolution; 1 collapses to a global gain.
    gain_bins: int = 8
    #: Spatial gain field resolution; 0 disables the spatial field.
    spatial_grid: int = 10
    #: How much of the fitted correction to apply; the fitted value is 1.0.
    strength: float = 1.0
    #: Source level above which the correction fades back to the source.
    highlight_protect: int = 245
    #: Log-brightness tolerance for dropping exposure-mismatched pairs.
    drop_exposure_outliers: float = .7
    #: Correspondences required per gain bin and per spatial cell.
    min_gain_bin_samples: int = 150
    min_spatial_cell_samples: int = 40
    #: Sampled frames and correspondences required to attempt a fit at all.
    min_sampled_frames: int = 8
    min_matched_samples: int = 500
    #: Sampling block size in sampled frames; alternating blocks train and validate.
    train_validation_block_size: int = 4
    #: Smallest held-out improvement, and largest tolerated regressions.
    min_improvement: float = .2
    max_p95_regression: float = 1.05
    max_clipped_increase: float = .005
    #: Baseline median CIEDE2000 below which the eyes are already consistent.
    apply_threshold: float = 3.0

    def __post_init__(self) -> None:
        if self.sample_step < 1:
            raise ValueError("sample_step must be at least 1")
        if not 0 < self.match_scale <= 1:
            raise ValueError("match_scale must be in (0, 1]")
        if self.gain_bins < 1:
            raise ValueError("gain_bins must be at least 1")
        if self.spatial_grid < 0:
            raise ValueError("spatial_grid must be non-negative")
        if self.strength < 0:
            raise ValueError("strength must be non-negative")
        if not 0 <= self.highlight_protect <= 255:
            raise ValueError("highlight_protect must be in [0, 255]")
        if self.train_validation_block_size < 1:
            raise ValueError("train_validation_block_size must be at least 1")


def srgb_to_linear(values: np.ndarray) -> np.ndarray:
    """Convert gamma-encoded sRGB in [0, 1] to linear light."""
    values = np.asarray(values, dtype=np.float64)
    return np.where(values <= .04045, values / 12.92, ((values + .055) / 1.055) ** 2.4)


def linear_to_srgb(values: np.ndarray) -> np.ndarray:
    """Convert linear light to gamma-encoded sRGB in [0, 1]."""
    values = np.clip(np.asarray(values, dtype=np.float64), 0.0, 1.0)
    return np.where(values <= .0031308, 12.92 * values, 1.055 * values ** (1 / 2.4) - .055)


def srgb_to_lab(rgb: np.ndarray) -> np.ndarray:
    """Convert gamma-encoded sRGB in [0, 1] to CIE L*a*b* (D65, 2 degree)."""
    linear = srgb_to_linear(np.asarray(rgb, dtype=np.float64))
    xyz = (linear @ _XYZ_FROM_SRGB.T) / _WHITE_XYZ
    delta = 6 / 29
    f = np.where(xyz > delta ** 3, np.cbrt(xyz), xyz / (3 * delta ** 2) + 4 / 29)
    return np.c_[116 * f[:, 1] - 16, 500 * (f[:, 0] - f[:, 1]), 200 * (f[:, 1] - f[:, 2])]


def ciede2000(lab1: np.ndarray, lab2: np.ndarray) -> np.ndarray:
    """Vectorized CIEDE2000 colour difference (Sharma et al. formulation)."""
    l1, a1, b1 = np.asarray(lab1, dtype=np.float64).T
    l2, a2, b2 = np.asarray(lab2, dtype=np.float64).T
    c1, c2 = np.hypot(a1, b1), np.hypot(a2, b2)
    c_bar = (c1 + c2) / 2
    g = (1 - np.sqrt(c_bar ** 7 / (c_bar ** 7 + 25 ** 7))) / 2
    a1p, a2p = (1 + g) * a1, (1 + g) * a2
    c1p, c2p = np.hypot(a1p, b1), np.hypot(a2p, b2)
    h1p = np.where(c1p == 0, 0.0, np.degrees(np.arctan2(b1, a1p)) % 360)
    h2p = np.where(c2p == 0, 0.0, np.degrees(np.arctan2(b2, a2p)) % 360)
    dlp, dcp = l2 - l1, c2p - c1p
    dhp = h2p - h1p
    dhp = np.where(c1p * c2p == 0, 0.0,
                   np.where(dhp > 180, dhp - 360, np.where(dhp < -180, dhp + 360, dhp)))
    dh = 2 * np.sqrt(c1p * c2p) * np.sin(np.radians(dhp / 2))
    l_bar, c_barp = (l1 + l2) / 2, (c1p + c2p) / 2
    h_sum = h1p + h2p
    h_bar = np.where(c1p * c2p == 0, h_sum,
                     np.where(np.abs(h1p - h2p) <= 180, h_sum / 2,
                              np.where(h_sum < 360, (h_sum + 360) / 2, (h_sum - 360) / 2)))
    t = (1 - .17 * np.cos(np.radians(h_bar - 30)) + .24 * np.cos(np.radians(2 * h_bar))
         + .32 * np.cos(np.radians(3 * h_bar + 6)) - .20 * np.cos(np.radians(4 * h_bar - 63)))
    d_theta = 30 * np.exp(-(((h_bar - 275) / 25) ** 2))
    rc = 2 * np.sqrt(c_barp ** 7 / (c_barp ** 7 + 25 ** 7))
    sl = 1 + .015 * (l_bar - 50) ** 2 / np.sqrt(20 + (l_bar - 50) ** 2)
    sc = 1 + .045 * c_barp
    sh = 1 + .015 * c_barp * t
    rt = -np.sin(np.radians(2 * d_theta)) * rc
    return np.sqrt((dlp / sl) ** 2 + (dcp / sc) ** 2 + (dh / sh) ** 2
                   + rt * (dcp / sc) * (dh / sh))


def _match_frame_pairs(
    reference_bgr: np.ndarray,
    source_bgr: np.ndarray,
    detector: cv2.Feature2D,
    match_scale: float,
    max_level: int | None,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Match one stereo pair and return source pixels, reference pixels, positions.

    The source (right) coordinates are reported because the spatial gain field is
    evaluated in the image that the correction is applied to; the two eyes are
    offset relative to each other, so reference coordinates would not line up.
    """
    if match_scale != 1:
        reference_match = cv2.resize(
            reference_bgr, None, fx=match_scale, fy=match_scale, interpolation=cv2.INTER_AREA
        )
        source_match = cv2.resize(
            source_bgr, None, fx=match_scale, fy=match_scale, interpolation=cv2.INTER_AREA
        )
    else:
        reference_match, source_match = reference_bgr, source_bgr

    empty = (np.empty((0, 3)), np.empty((0, 3)), np.empty((0, 2)))
    detections = [
        detector.detectAndCompute(cv2.cvtColor(image, cv2.COLOR_BGR2GRAY), None)
        for image in (reference_match, source_match)
    ]
    (key_reference, reference_descriptor), (key_source, source_descriptor) = detections
    if reference_descriptor is None or source_descriptor is None:
        return empty

    pairs = cv2.BFMatcher(cv2.NORM_L2).knnMatch(reference_descriptor, source_descriptor, k=2)
    inverse_scale = 1.0 / match_scale
    source_colors, reference_colors, positions = [], [], []
    for pair in pairs:
        if len(pair) != 2:
            continue
        best, second = pair
        if best.distance >= .7 * second.distance:
            continue
        x, y = key_reference[best.queryIdx].pt
        u, v = key_source[best.trainIdx].pt
        x, y, u, v = (int(round(value * inverse_scale)) for value in (x, y, u, v))
        x = min(max(x, 0), reference_bgr.shape[1] - 1)
        y = min(max(y, 0), reference_bgr.shape[0] - 1)
        u = min(max(u, 0), source_bgr.shape[1] - 1)
        v = min(max(v, 0), source_bgr.shape[0] - 1)
        if abs(x - u) > 250 or abs(y - v) > 250:
            continue
        level_reference = float(np.mean(reference_bgr[y, x]))
        level_source = float(np.mean(source_bgr[v, u]))
        if level_reference <= 5 or level_source <= 5:
            continue
        if max_level is not None and (level_reference >= max_level or level_source >= max_level):
            continue
        reference_colors.append(reference_bgr[y, x][::-1] / 255.0)
        source_colors.append(source_bgr[v, u][::-1] / 255.0)
        positions.append((u / source_bgr.shape[1], v / source_bgr.shape[0]))
    if not source_colors:
        return empty
    return (
        np.asarray(source_colors),
        np.asarray(reference_colors),
        np.asarray(positions),
    )


def exposure_outlier_mask(source: np.ndarray, reference: np.ndarray, tolerance: float) -> np.ndarray:
    """Flag pairs whose brightness ratio is far from the fitted gain.

    A pair whose source is much brighter than the expected ratio is an
    over-exposed sample: it reflects an exposure mismatch, not a tone
    difference, and pulling it into the fit biases the highlight end.
    """
    if tolerance <= 0 or not len(source):
        return np.ones(len(source), dtype=bool)
    gain = np.sum(source * reference, 0) / (np.sum(source * source, 0) + 1e-8)
    expected = float(np.log(1 / np.clip(gain, 1e-6, None)).mean())
    deviation = np.abs(np.log(np.maximum(source.mean(1), 1e-6))
                       - np.log(np.maximum(reference.mean(1), 1e-6)) - expected)
    return deviation <= tolerance


def _fit_gain_bins(
    source: np.ndarray,
    reference: np.ndarray,
    bins: int,
    min_samples: int,
) -> tuple[tuple[np.ndarray, ...], tuple[np.ndarray, ...]]:
    """Fit a per-channel gain curve from binned correspondences.

    Each channel is binned on its *own* value, which keeps the correction
    expressible as one 256-entry table per channel. A channel with too few
    populated bins falls back to its single global least-squares gain.
    """
    bins = max(2, min(bins, len(source) // (4 * min_samples) or 2))
    anchors: list[np.ndarray] = []
    gains: list[np.ndarray] = []
    for channel in range(3):
        values = source[:, channel]
        global_gain = float(np.sum(values * reference[:, channel])
                            / (np.sum(values ** 2) + 1e-8))
        edges = np.unique(np.quantile(values, np.linspace(0, 1, bins + 1)))
        channel_anchors, channel_gains = [], []
        for low, high in zip(edges[:-1], edges[1:]):
            mask = (values >= low) & (values <= high)
            if mask.sum() < min_samples:
                continue
            channel_anchors.append(float(np.median(values[mask])))
            channel_gains.append(float(np.sum(values[mask] * reference[mask, channel])
                                       / (np.sum(values[mask] ** 2) + 1e-8)))
        if not channel_anchors:
            anchors.append(np.asarray([float(np.median(values))]))
            gains.append(np.asarray([global_gain]))
            continue
        anchors.append(np.asarray(channel_anchors))
        gains.append(np.asarray(channel_gains))
    return tuple(anchors), tuple(gains)


def _fit_spatial_gain(
    source: np.ndarray,
    reference: np.ndarray,
    positions: np.ndarray,
    anchors: tuple[np.ndarray, ...],
    gains: tuple[np.ndarray, ...],
    grid: int,
    min_samples: int,
    smooth: float = 1.2,
) -> np.ndarray:
    """Fit a smooth spatial gain field on the residual left by the gain curves.

    The stereo mismatch is not uniform across the frame: the gain needed on one
    side of the image differs from the other by more than a factor of two, and no
    global model can express that. Returns a small ``(grid, grid, 3)`` RGB field
    that the corrector upsamples to the frame.
    """
    corrected = np.empty_like(source)
    for channel in range(3):
        corrected[:, channel] = source[:, channel] * np.interp(
            source[:, channel], anchors[channel], np.clip(gains[channel], .2, 3))
    residual = reference / np.maximum(corrected, 1e-6)
    cell_x = np.clip((positions[:, 0] * grid).astype(int), 0, grid - 1)
    cell_y = np.clip((positions[:, 1] * grid).astype(int), 0, grid - 1)
    cells = np.ones((grid, grid, 3))
    populated = np.zeros((grid, grid), dtype=bool)
    for row in range(grid):
        for column in range(grid):
            mask = (cell_y == row) & (cell_x == column)
            if mask.sum() < min_samples:
                continue
            cells[row, column] = np.median(residual[mask], 0)
            populated[row, column] = True
    if not populated.any():
        return cells
    # Empty cells are filled from the nearest populated one: a single global
    # value leaves holes that the upsampled field turns into blotches across the
    # frame, which costs far more than the field gains.
    filled_y, filled_x = np.where(populated)
    for row, column in zip(*np.where(~populated)):
        nearest = np.argmin((filled_y - row) ** 2 + (filled_x - column) ** 2)
        cells[row, column] = cells[filled_y[nearest], filled_x[nearest]]
    for channel in range(3):
        cells[:, :, channel] = cv2.GaussianBlur(cells[:, :, channel], (0, 0), smooth)
    return cells


def _sample_spatial_gain(cells: np.ndarray, positions: np.ndarray) -> np.ndarray:
    """Sample the fitted cell field at normalized positions the way the corrector does."""
    grid = cells.shape[0]
    coordinates = np.clip(np.asarray(positions, dtype=np.float64), 0.0, 1.0) * grid - 0.5
    lower = np.clip(np.floor(coordinates).astype(int), 0, grid - 1)
    upper = np.clip(lower + 1, 0, grid - 1)
    fraction = np.clip(coordinates - lower, 0.0, 1.0)
    lower_x, upper_x = lower[:, 0], upper[:, 0]
    lower_y, upper_y = lower[:, 1], upper[:, 1]
    fx = fraction[:, 0:1]
    fy = fraction[:, 1:2]
    top = cells[lower_y, lower_x] * (1 - fx) + cells[lower_y, upper_x] * fx
    bottom = cells[upper_y, lower_x] * (1 - fx) + cells[upper_y, upper_x] * fx
    return top * (1 - fy) + bottom * fy


class _ColorModel:
    """Fitted gain curves plus an optional spatial field."""

    def __init__(
        self,
        anchors: tuple[np.ndarray, ...],
        gains: tuple[np.ndarray, ...],
        cells: np.ndarray | None,
        strength: float,
    ) -> None:
        self.anchors = tuple(np.asarray(anchor, dtype=np.float64) for anchor in anchors)
        self.gains = tuple(
            np.clip(np.asarray(gain, dtype=np.float64), .2, 3) for gain in gains
        )
        self.cells = None if cells is None else np.asarray(cells, dtype=np.float64)
        self.strength = float(strength)

    @property
    def _scaled_gains(self) -> tuple[np.ndarray, ...]:
        return tuple(1 + self.strength * (gain - 1) for gain in self.gains)

    def evaluate(self, source_linear: np.ndarray, positions: np.ndarray) -> np.ndarray:
        """Evaluate the fitted model at specific linear-light samples."""
        corrected = np.empty_like(source_linear)
        scaled = self._scaled_gains
        for channel in range(3):
            corrected[:, channel] = source_linear[:, channel] * np.interp(
                source_linear[:, channel], self.anchors[channel], scaled[channel])
        if self.cells is not None and len(positions):
            corrected *= _sample_spatial_gain(self.cells, positions)
        return corrected

    def corrector(self, highlight_protect: int):
        """Build the per-frame transform, preferring the cheapest correct path."""
        if self.cells is None:
            return _highlight_protect(_channel_table_corrector(self), highlight_protect)
        return _highlight_protect(_spatial_corrector(self), highlight_protect)

    def fingerprint(self) -> str:
        payload = {
            "algorithm_version": ALGORITHM_VERSION,
            "strength": round(self.strength, 6),
            "anchors": [[round(float(value), 6) for value in anchor] for anchor in self.anchors],
            "gains": [[round(float(value), 6) for value in gain] for gain in self.gains],
            "spatial_gain": None if self.cells is None else np.round(self.cells, 6).tolist(),
        }
        encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(encoded).hexdigest()


def _channel_table_corrector(model: _ColorModel):
    """Collapse channel-separable gains into one 256-entry table per channel."""
    linear = _SRGB8_TO_LINEAR.astype(np.float64)
    columns = []
    scaled = model._scaled_gains
    for channel in range(3):
        curve = np.interp(linear, model.anchors[channel], scaled[channel])
        indices = np.rint(np.clip(curve * linear, 0, 1)
                          * (_LINEAR_GRID_SIZE - 1)).astype(np.uint16)
        columns.append(_LINEAR_TO_SRGB8[indices])
    table = np.ascontiguousarray(np.stack(columns, 1)[:, ::-1]).reshape(1, 256, 3)

    def correct(image: np.ndarray) -> np.ndarray:
        return cv2.LUT(np.ascontiguousarray(image), table)

    return correct


def _spatial_corrector(model: _ColorModel):
    """Gain curves plus a smooth spatial gain field, applied in linear light.

    The spatial multiply has to happen in linear light, so this path runs
    srgb->linear table, per-pixel multiply, and a 16-bit index back into the
    inverse transfer table. OpenCV's ``LUT`` only accepts 8-bit sources, so the
    inverse step uses NumPy indexing instead.
    """
    linear = _SRGB8_TO_LINEAR.astype(np.float64)
    columns = []
    scaled = model._scaled_gains
    for channel in range(3):
        # The table holds the gain-corrected *linear* value, not the gain: the
        # spatial field multiplies it afterwards, still in linear light.
        columns.append((np.interp(linear, model.anchors[channel], scaled[channel])
                        * linear).astype(np.float32))
    linear_table = np.ascontiguousarray(np.stack(columns, 1)[:, ::-1]).reshape(1, 256, 3)
    small = np.ascontiguousarray(model.cells[:, :, ::-1], dtype=np.float32)
    cache: dict[tuple[int, int], np.ndarray] = {}

    def correct(image: np.ndarray) -> np.ndarray:
        image = np.ascontiguousarray(image)
        height, width = image.shape[:2]
        gain_map = cache.get((height, width))
        if gain_map is None:
            gain_map = np.ascontiguousarray(
                cv2.resize(small, (width, height), interpolation=cv2.INTER_LINEAR))
            cache[(height, width)] = gain_map
        linear_image = cv2.LUT(image, linear_table)
        indices = cv2.multiply(linear_image, gain_map, scale=65535.0, dtype=cv2.CV_16U)
        return _LINEAR_TO_SRGB8[indices]

    return correct


def _highlight_protect(correct, level: int):
    """Fade the correction back to the source where pixels are saturated.

    A multiplicative correction cannot restore a clipped highlight: it tints a
    blown-out white instead. Fading to the source above ``level`` (on the
    smallest channel) leaves those pixels alone. No-op when ``level <= 0``.
    """
    if level <= 0:
        return correct
    ramp = np.clip((np.arange(256) - level) / (255 - level), 0, 1)
    weight_lut = np.rint(ramp * 255).astype(np.uint8)

    def protected(image: np.ndarray) -> np.ndarray:
        corrected = correct(image)
        blue, green, red = cv2.split(image)
        weight = cv2.merge(
            [cv2.LUT(cv2.min(cv2.min(blue, green), red), weight_lut)] * 3)
        return cv2.add(cv2.multiply(corrected, cv2.bitwise_not(weight), scale=1 / 255.0),
                       cv2.multiply(image, weight, scale=1 / 255.0))

    return protected


#: Metrics reported when no usable measurement exists.
_EMPTY_METRICS = {
    "ciede2000_median": 0.0,
    "ciede2000_p95": 0.0,
    "linear_mae_median": 0.0,
    "clipped_fraction": 0.0,
}


def _measure(source_srgb: np.ndarray, reference_srgb: np.ndarray) -> dict[str, float]:
    """Perceptual and photometric disagreement on one set of matched pairs."""
    source_lab = srgb_to_lab(source_srgb)
    reference_lab = srgb_to_lab(reference_srgb)
    delta_e = ciede2000(source_lab, reference_lab)
    source_linear = srgb_to_linear(source_srgb)
    reference_linear = srgb_to_linear(reference_srgb)
    return {
        "ciede2000_median": float(np.median(delta_e)),
        "ciede2000_p95": float(np.percentile(delta_e, 95)),
        "linear_mae_median": float(np.median(np.mean(np.abs(source_linear - reference_linear), 1))),
        "clipped_fraction": float(np.mean(np.max(source_srgb, axis=1) >= UNSATURATED_LEVEL / 255.0)),
    }


@dataclass(frozen=True)
class ColorCorrectionReport:
    """Immutable, serializable outcome of one calibration."""

    algorithm_version: str
    decision: str
    reason: str
    reference_eye: str
    sampled_frames: int
    matches_before_filter: int
    matches_after_filter: int
    train_samples: int
    validation_samples: int
    baseline: dict[str, float]
    corrected: dict[str, float]
    model: dict[str, object]

    def as_dict(self) -> dict[str, object]:
        """Return the full report, including the fitted model parameters."""
        payload: dict[str, object] = {
            "algorithm_version": self.algorithm_version,
            "reference_eye": self.reference_eye,
            "decision": self.decision,
            "reason": self.reason,
            "sampled_frames": self.sampled_frames,
            "matches_before_filter": self.matches_before_filter,
            "matches_after_filter": self.matches_after_filter,
            "train_samples": self.train_samples,
            "validation_samples": self.validation_samples,
        }
        for prefix, metrics in (("baseline", self.baseline), ("corrected", self.corrected)):
            for name, value in metrics.items():
                payload[f"{prefix}_{name}"] = value
        payload["model"] = dict(self.model)
        return payload


class ColorCorrectionPlan:
    """The fixed correction for one recording, or the reason none was applied."""

    def __init__(
        self,
        report: ColorCorrectionReport,
        corrector=None,
    ) -> None:
        self._report = report
        self._corrector = corrector

    @property
    def decision(self) -> str:
        return self._report.decision

    @property
    def reason(self) -> str:
        return self._report.reason

    @property
    def applied(self) -> bool:
        """Whether :meth:`apply` may be used on this recording."""
        return self._report.decision == DECISION_APPLIED and self._corrector is not None

    @property
    def report(self) -> ColorCorrectionReport:
        return self._report

    @property
    def summary(self) -> dict[str, object]:
        """Return the report without the fitted model arrays."""
        payload = self._report.as_dict()
        payload.pop("model", None)
        model = self._report.model
        payload["model_sha256"] = model.get("sha256")
        payload["gain_bins"] = model.get("gain_bins")
        payload["spatial_grid"] = model.get("spatial_grid")
        payload["strength"] = model.get("strength")
        payload["highlight_protect"] = model.get("highlight_protect")
        return payload

    def apply(self, source_bgr: np.ndarray) -> np.ndarray:
        """Apply the fixed correction to one right-eye BGR frame."""
        if not self.applied:
            raise RuntimeError(
                f"color correction plan is not applied: {self.decision} ({self.reason})")
        return self._corrector(source_bgr)


class ColorConsistencyCalibrator:
    """Accumulate stereo correspondences, then fit and validate one model.

    The calibrator owns sampling, matching, outlier rejection, the train and
    validation split, the fit, and the apply/skip decision, so the caller only
    feeds decoded frames and then uses the returned plan.
    """

    def __init__(self, config: ColorConsistencyConfig | None = None) -> None:
        self.config = config or ColorConsistencyConfig()
        self._detector = cv2.SIFT_create(nfeatures=self.config.max_match_features)
        self._source_samples: list[np.ndarray] = []
        self._reference_samples: list[np.ndarray] = []
        self._sample_positions: list[np.ndarray] = []
        self._sample_blocks: list[int] = []
        self._sampled_frames = 0
        self._matches_before_filter = 0
        self._sample_ordinals = 0
        self._plan: ColorCorrectionPlan | None = None

    @property
    def sampled_frames(self) -> int:
        """Number of frame pairs that produced at least one correspondence."""
        return self._sampled_frames

    @property
    def observed_samples(self) -> int:
        """Number of retained correspondences, before outlier rejection."""
        return self._matches_before_filter

    def observe(self, frame_index: int, reference_bgr: np.ndarray, source_bgr: np.ndarray) -> bool:
        """Feed one aligned stereo pair; returns whether it was sampled."""
        if self._plan is not None:
            raise RuntimeError("calibrator already fitted")
        if frame_index < 0:
            raise ValueError("frame_index must be non-negative")
        if reference_bgr.shape != source_bgr.shape:
            raise ValueError("reference and source frames must share one shape")
        if frame_index % self.config.sample_step:
            return False
        source, reference, positions = _match_frame_pairs(
            reference_bgr,
            source_bgr,
            self._detector,
            self.config.match_scale,
            None,
        )
        self._sample_ordinals += 1
        if not len(source):
            return False
        self._sampled_frames += 1
        room = self.config.max_samples - self._matches_before_filter
        if room <= 0:
            return True
        accepted = min(room, len(source))
        self._source_samples.append(source[:accepted])
        self._reference_samples.append(reference[:accepted])
        self._sample_positions.append(positions[:accepted])
        self._sample_blocks.append(
            np.full(accepted, (self._sample_ordinals - 1) // self.config.train_validation_block_size,
                    dtype=np.int64))
        self._matches_before_filter += accepted
        return True

    def fit(self) -> ColorCorrectionPlan:
        """Fit, validate, and freeze the correction; idempotent."""
        if self._plan is not None:
            return self._plan
        config = self.config
        source = (np.concatenate(self._source_samples) if self._source_samples
                  else np.empty((0, 3)))
        reference = (np.concatenate(self._reference_samples) if self._reference_samples
                     else np.empty((0, 3)))
        positions = (np.concatenate(self._sample_positions) if self._sample_positions
                     else np.empty((0, 2)))
        blocks = (np.concatenate(self._sample_blocks) if self._sample_blocks
                  else np.empty(0, dtype=np.int64))

        if self._sampled_frames < config.min_sampled_frames or len(source) < config.min_matched_samples:
            self._plan = self._skip(
                DECISION_INSUFFICIENT,
                "insufficient_correspondences",
                source, reference, 0, len(source),
            )
            return self._plan

        keep = exposure_outlier_mask(
            srgb_to_linear(source), srgb_to_linear(reference), config.drop_exposure_outliers)
        source, reference, positions, blocks = source[keep], reference[keep], positions[keep], blocks[keep]
        after_filter = len(source)
        train = blocks % 2 == 0
        # Adjacent frames are highly correlated, so blocks alternate between
        # train and validation instead of splitting the recording in half.
        if train.sum() < 2 or (~train).sum() < 2:
            self._plan = self._skip(
                DECISION_INSUFFICIENT, "insufficient_correspondences",
                source, reference, after_filter, int((~train).sum()),
                train_mask=train,
            )
            return self._plan

        baseline_validation = _measure(source[~train], reference[~train])
        baseline_train = _measure(source[train], reference[train])
        if baseline_validation["ciede2000_median"] < config.apply_threshold:
            self._plan = self._skip(
                DECISION_NOT_NEEDED, "baseline_within_threshold",
                source, reference, after_filter, int((~train).sum()),
                baseline=baseline_validation, train_baseline=baseline_train,
                train_mask=train,
            )
            return self._plan

        source_linear = srgb_to_linear(source)
        reference_linear = srgb_to_linear(reference)
        anchors, gains = _fit_gain_bins(
            source_linear[train], reference_linear[train],
            config.gain_bins, config.min_gain_bin_samples)
        cells = None
        if config.spatial_grid > 0:
            cells = _fit_spatial_gain(
                source_linear[train], reference_linear[train], positions[train],
                anchors, gains, config.spatial_grid, config.min_spatial_cell_samples)
        model = _ColorModel(anchors, gains, cells, config.strength)
        if not _model_is_finite(model):
            self._plan = self._skip(
                DECISION_REJECTED, "invalid_model",
                source, reference, after_filter, int((~train).sum()),
                baseline=baseline_validation, train_mask=train,
            )
            return self._plan

        corrected_linear = model.evaluate(source_linear, positions)
        corrected_srgb = linear_to_srgb(corrected_linear)
        corrected_validation = _measure(corrected_srgb[~train], reference[~train])
        corrected_train = _measure(corrected_srgb[train], reference[train])

        reason = self._rejection_reason(baseline_validation, corrected_validation)
        model_payload = {
            "gain_bins": config.gain_bins,
            "spatial_grid": config.spatial_grid,
            "strength": config.strength,
            "highlight_protect": config.highlight_protect,
            "anchors": [anchor.tolist() for anchor in model.anchors],
            "gains": [gain.tolist() for gain in model.gains],
            "spatial_gain": None if model.cells is None else np.round(model.cells, 6).tolist(),
            "sha256": model.fingerprint(),
        }
        if reason is not None:
            self._plan = ColorCorrectionPlan(self._build_report(
                DECISION_REJECTED, reason, after_filter, int(train.sum()), int((~train).sum()),
                baseline_validation, corrected_validation, model_payload))
            return self._plan

        self._plan = ColorCorrectionPlan(
            self._build_report(
                DECISION_APPLIED, "validation_improved", after_filter, int(train.sum()),
                int((~train).sum()), baseline_validation, corrected_validation, model_payload),
            model.corrector(config.highlight_protect),
        )
        return self._plan

    def _rejection_reason(
        self,
        baseline: dict[str, float],
        corrected: dict[str, float],
    ) -> str | None:
        config = self.config
        if corrected["ciede2000_median"] > baseline["ciede2000_median"] * (1 - config.min_improvement):
            return "validation_not_improved"
        if corrected["ciede2000_p95"] > baseline["ciede2000_p95"] * config.max_p95_regression:
            return "validation_p95_regressed"
        if (corrected["clipped_fraction"] - baseline["clipped_fraction"]) > config.max_clipped_increase:
            return "clipped_highlight_increase"
        return None

    def _build_report(
        self,
        decision: str,
        reason: str,
        after_filter: int,
        train_samples: int,
        validation_samples: int,
        baseline: dict[str, float],
        corrected: dict[str, float],
        model: dict[str, object],
        train_baseline: dict[str, float] | None = None,
    ) -> ColorCorrectionReport:
        empty = {"ciede2000_median": 0.0, "ciede2000_p95": 0.0,
                 "linear_mae_median": 0.0, "clipped_fraction": 0.0}
        payload = dict(model)
        payload["train_baseline_ciede2000_median"] = (
            float(train_baseline["ciede2000_median"]) if train_baseline else 0.0)
        return ColorCorrectionReport(
            algorithm_version=ALGORITHM_VERSION,
            decision=decision,
            reason=reason,
            reference_eye="left",
            sampled_frames=self._sampled_frames,
            matches_before_filter=self._matches_before_filter,
            matches_after_filter=after_filter,
            train_samples=train_samples,
            validation_samples=validation_samples,
            baseline=dict(baseline or empty),
            corrected=dict(corrected or empty),
            model=payload,
        )

    def _skip(
        self,
        decision: str,
        reason: str,
        source: np.ndarray,
        reference: np.ndarray,
        after_filter: int,
        validation_samples: int,
        baseline: dict[str, float] | None = None,
        train_baseline: dict[str, float] | None = None,
        train_mask: np.ndarray | None = None,
    ) -> ColorCorrectionPlan:
        metrics = baseline
        if metrics is None:
            metrics = _measure(source, reference) if len(source) else _EMPTY_METRICS
        if train_baseline is None and train_mask is not None and len(source):
            train_baseline = _measure(source[train_mask], reference[train_mask])
        return ColorCorrectionPlan(self._build_report(
            decision, reason, after_filter,
            int(train_mask.sum()) if train_mask is not None else 0,
            validation_samples, metrics, metrics, {}, train_baseline))


def _model_is_finite(model: _ColorModel) -> bool:
    for anchor, gain in zip(model.anchors, model.gains):
        if not np.all(np.isfinite(anchor)) or not np.all(np.isfinite(gain)):
            return False
    if model.cells is not None and not np.all(np.isfinite(model.cells)):
        return False
    return True
