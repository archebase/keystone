# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

import json
from pathlib import Path
import sys
import tempfile
import unittest

import cv2
import numpy as np


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from color_consistency import (  # noqa: E402
    ALGORITHM_VERSION,
    DECISION_APPLIED,
    DECISION_INSUFFICIENT,
    DECISION_NOT_NEEDED,
    DECISION_REJECTED,
    ColorConsistencyCalibrator,
    ColorConsistencyConfig,
    ciede2000,
    srgb_to_lab,
)


def textured_scene(width: int = 480, height: int = 320, seed: int = 7) -> np.ndarray:
    """Build a high-contrast textured BGR image with repeatable features."""
    rng = np.random.default_rng(seed)
    image = np.zeros((height, width, 3), dtype=np.float64)
    yy, xx = np.mgrid[0:height, 0:width]
    image[..., 0] = 60 + 90 * xx / width
    image[..., 1] = 70 + 80 * yy / height
    image[..., 2] = 110 + 60 * (1 - xx / width)
    for _ in range(70):
        center = (
            int(rng.integers(20, width - 20)),
            int(rng.integers(20, height - 20)),
        )
        radius = int(rng.integers(6, 18))
        level = float(rng.integers(20, 200))
        cv2.circle(
            image,
            center,
            radius,
            (level, min(200.0, level + 40), max(15.0, level - 50)),
            -1,
        )
    for _ in range(35):
        origin = (
            int(rng.integers(0, width - 70)),
            int(rng.integers(0, height - 70)),
        )
        size = (int(rng.integers(20, 60)), int(rng.integers(20, 60)))
        cv2.rectangle(
            image,
            origin,
            (origin[0] + size[0], origin[1] + size[1]),
            (
                float(rng.integers(10, 200)),
                float(rng.integers(10, 200)),
                float(rng.integers(10, 200)),
            ),
            -1,
        )
    # Keep every level clear of the clipped-highlight metric so the fixture
    # measures photometric mismatch rather than saturation.
    return cv2.GaussianBlur(np.clip(image, 0, 200).astype(np.uint8), (0, 0), 1.0)


def mismatched_source(
    reference: np.ndarray,
    gains: tuple[float, float, float] = (.65, 1.35, 1.25),
    spatial: float = .35,
    disparity: int = 0,
) -> np.ndarray:
    """Return the source eye: gained, spatially graded, and optionally shifted."""
    height, width = reference.shape[:2]
    field = 1.0 + spatial * (.5 - np.linspace(0, 1, width)[None, :, None])
    values = reference.astype(np.float64) / 255.0
    values = values * np.asarray(gains)[None, None, :] * field
    source = np.clip(np.rint(values * 255.0), 0, 255).astype(np.uint8)
    if disparity:
        matrix = np.float32([[1, 0, disparity], [0, 1, 0]])
        source = cv2.warpAffine(
            source, matrix, (width, height), borderMode=cv2.BORDER_REPLICATE)
    return source


def scene_frames(count: int, disparity: int = 12):
    """Yield ``count`` slightly varying reference/source frame pairs."""
    reference = textured_scene()
    for index in range(count):
        faded = cv2.convertScaleAbs(reference, alpha=1.0 - .002 * index, beta=-.5 * index)
        yield faded, mismatched_source(faded, disparity=disparity)


def calibration_config(**overrides) -> ColorConsistencyConfig:
    values = {
        "sample_step": 1,
        "match_scale": 1.0,
        "min_sampled_frames": 8,
        "min_matched_samples": 50,
        "min_gain_bin_samples": 10,
        "min_spatial_cell_samples": 5,
        "spatial_grid": 4,
        "train_validation_block_size": 2,
    }
    values.update(overrides)
    return ColorConsistencyConfig(**values)


def fitted_plan(**overrides):
    calibrator = ColorConsistencyCalibrator(calibration_config(**overrides))
    for index, (reference, source) in enumerate(scene_frames(12)):
        calibrator.observe(index, reference, source)
    return calibrator.fit()


class ColorConsistencyFitTest(unittest.TestCase):
    def test_fits_and_improves_held_out_colour_difference(self) -> None:
        plan = fitted_plan()

        self.assertEqual(plan.decision, DECISION_APPLIED)
        self.assertEqual(plan.reason, "validation_improved")
        self.assertTrue(plan.applied)
        report = plan.report
        self.assertGreater(report.sampled_frames, 8)
        self.assertGreater(report.matches_before_filter, 0)
        self.assertEqual(
            report.train_samples + report.validation_samples,
            report.matches_after_filter,
        )
        self.assertGreater(report.train_samples, 0)
        self.assertGreater(report.validation_samples, 0)
        self.assertLess(
            report.corrected["ciede2000_median"],
            report.baseline["ciede2000_median"] * .5,
        )
        self.assertLess(
            report.corrected["linear_mae_median"],
            report.baseline["linear_mae_median"] * .5,
        )

    def test_correction_reduces_pixel_difference_for_aligned_eyes(self) -> None:
        reference = textured_scene()
        source = mismatched_source(reference)
        calibrator = ColorConsistencyCalibrator(calibration_config())
        for index in range(12):
            calibrator.observe(index, reference, source)
        plan = calibrator.fit()

        corrected = plan.apply(source)
        self.assertEqual(corrected.shape, source.shape)
        self.assertEqual(corrected.dtype, np.uint8)
        before = float(np.mean(np.abs(source.astype(np.float64) - reference)))
        after = float(np.mean(np.abs(corrected.astype(np.float64) - reference)))
        self.assertLess(after, before * .4)

    def test_table_corrector_path_matches_held_out_metrics(self) -> None:
        plan = fitted_plan(spatial_grid=0)

        self.assertEqual(plan.decision, DECISION_APPLIED)
        self.assertIsNone(plan.report.model["spatial_gain"])
        corrected = plan.apply(textured_scene())
        self.assertEqual(corrected.shape, textured_scene().shape)

    def test_skips_when_eyes_are_already_consistent(self) -> None:
        calibrator = ColorConsistencyCalibrator(calibration_config())
        reference = textured_scene()
        for index in range(12):
            calibrator.observe(index, reference, reference.copy())
        plan = calibrator.fit()

        self.assertEqual(plan.decision, DECISION_NOT_NEEDED)
        self.assertEqual(plan.reason, "baseline_within_threshold")
        self.assertFalse(plan.applied)
        with self.assertRaises(RuntimeError):
            plan.apply(reference)

    def test_reports_insufficient_correspondences_without_texture(self) -> None:
        calibrator = ColorConsistencyCalibrator(calibration_config())
        flat = np.full((120, 160, 3), 128, dtype=np.uint8)
        for index in range(12):
            calibrator.observe(index, flat, flat)
        plan = calibrator.fit()

        self.assertEqual(plan.decision, DECISION_INSUFFICIENT)
        self.assertEqual(plan.reason, "insufficient_correspondences")
        self.assertFalse(plan.applied)
        self.assertEqual(plan.report.matches_after_filter, 0)

    def test_rejects_when_the_fit_cannot_beat_the_baseline(self) -> None:
        plan = fitted_plan(min_improvement=.999)

        self.assertEqual(plan.decision, DECISION_REJECTED)
        self.assertEqual(plan.reason, "validation_not_improved")
        self.assertFalse(plan.applied)

    def test_highlight_protect_keeps_clipped_pixels(self) -> None:
        plan = fitted_plan()
        self.assertTrue(plan.applied)
        frame = np.full((60, 80, 3), 180, dtype=np.uint8)
        frame[10:20, 10:20] = 255
        frame[30:40, 30:40] = 120
        corrected = plan.apply(frame)

        self.assertTrue(np.array_equal(corrected[10:20, 10:20], frame[10:20, 10:20]))
        self.assertFalse(np.array_equal(corrected[30:40, 30:40], frame[30:40, 30:40]))

    def test_same_input_produces_the_same_model(self) -> None:
        first = fitted_plan().report.model["sha256"]
        second = fitted_plan().report.model["sha256"]

        self.assertEqual(first, second)

    def test_summary_is_json_serializable_and_lists_the_algorithm(self) -> None:
        plan = fitted_plan()

        summary = plan.summary
        encoded = json.dumps(summary, sort_keys=True)
        self.assertIn(ALGORITHM_VERSION, encoded)
        self.assertNotIn("anchors", summary)
        self.assertEqual(summary["algorithm_version"], ALGORITHM_VERSION)
        self.assertEqual(summary["model_sha256"], plan.report.model["sha256"])
        full = json.dumps(plan.report.as_dict(), sort_keys=True)
        self.assertIn("anchors", full)
        temp_dir = tempfile.mkdtemp()
        path = Path(temp_dir) / "report.json"
        path.write_text(full, encoding="utf-8")
        self.assertGreater(path.stat().st_size, 0)

    def test_sampling_step_skips_unsampled_frames(self) -> None:
        calibrator = ColorConsistencyCalibrator(calibration_config(sample_step=3))
        for index, (reference, source) in enumerate(scene_frames(12)):
            calibrator.observe(index, reference, source)

        self.assertEqual(calibrator.sampled_frames, 4)

    def test_rejects_mismatched_reference_and_source_shapes(self) -> None:
        calibrator = ColorConsistencyCalibrator(calibration_config())
        with self.assertRaises(ValueError):
            calibrator.observe(0, np.zeros((10, 10, 3), np.uint8), np.zeros((11, 10, 3), np.uint8))
        with self.assertRaises(ValueError):
            calibrator.observe(-1, np.zeros((10, 10, 3), np.uint8), np.zeros((10, 10, 3), np.uint8))


class ColorConsistencyConfigTest(unittest.TestCase):
    def test_rejects_invalid_parameters(self) -> None:
        for overrides in (
            {"sample_step": 0},
            {"match_scale": 0.0},
            {"gain_bins": 0},
            {"spatial_grid": -1},
            {"strength": -1.0},
            {"highlight_protect": 256},
            {"train_validation_block_size": 0},
        ):
            with self.subTest(overrides=overrides):
                with self.assertRaises(ValueError):
                    ColorConsistencyConfig(**overrides)


class Ciede2000Test(unittest.TestCase):
    def test_identical_colours_have_zero_difference(self) -> None:
        lab = srgb_to_lab(np.asarray([[.2, .5, .8], [.9, .1, .3]]))
        self.assertAlmostEqual(float(np.max(ciede2000(lab, lab))), 0.0, places=9)

    def test_reference_pair_matches_published_value(self) -> None:
        # Sharma et al. reference pair 1: L*a*b* (50, 2.6772, -79.7751) vs
        # (50, 0, -82.7485) has a CIEDE2000 difference of 2.0425.
        first = np.asarray([[50.0, 2.6772, -79.7751]])
        second = np.asarray([[50.0, 0.0, -82.7485]])
        self.assertAlmostEqual(float(ciede2000(first, second)[0]), 2.0425, places=4)


if __name__ == "__main__":
    unittest.main()
