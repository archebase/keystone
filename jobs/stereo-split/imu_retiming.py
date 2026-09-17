#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Re-time DECXIN IMU samples onto the device's own rigid sampling grid.

The recorder writes ``/decxin/imu`` with the host time at which each message was
emitted and repeats one sample at every packet boundary, so a 600 Hz sensor
shows up as ~661 messages per second in bursts of eleven, and the payload header
carries a per-frame interpolation whose rate is about 13% off.

The image metadata column still carries the device's own microsecond timestamps
for one packet per camera frame, and that packet is enough to reconstruct the
whole stream: the device grid is rigidly locked to the camera frame strobe, so
its spacing, its offset from the frame's exposure end, and the number of grid
intervals per frame are constants that can be measured per file and then checked
frame by frame.

The module is validation first: the caller feeds every decoded frame barcode and
every source message, and :meth:`ImuRetimingCalibrator.resolve` either returns a
plan that covers the whole stream or refuses to retime it at all.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from imu_decoder import ImuFrame

#: Decision values reported by :meth:`ImuRetimingCalibrator.resolve`.
DECISION_APPLIED = "applied"
DECISION_SKIPPED = "skipped"

#: Algorithm identity recorded in the manifest.
ALGORITHM_VERSION = "decxin-imu-grid-v1"


@dataclass(frozen=True)
class ImuRetimingConfig:
    """Fixed validation thresholds for the device grid."""

    #: The device emits overlapping windows of this many samples.
    packet_size: int = 11
    #: Camera frames carry this many packets, and therefore this many grid intervals.
    packets_per_frame: int = 2
    #: Smallest number of decoded barcodes that can establish the grid constants.
    min_sampled_frames: int = 8
    #: How far a single frame's measurement may sit from the file's median.
    spacing_tolerance_us: float = 2.0
    anchor_tolerance_us: int = 200
    #: Device timestamps are microseconds with rounding jitter, so the grid is
    #: checked against a tolerance rather than for exact equality.
    timestamp_tolerance_us: int = 2
    #: How close a source sample must be to a barcode sample to count as a match.
    sample_tolerance_mg: float = 1.0
    #: Packets searched around the expected position for the first frame.
    search_window_packets: int = 64
    #: Accepted device-clock rate, in nanoseconds per device microsecond.
    min_clock_scale: float = 900.0
    max_clock_scale: float = 1100.0
    #: Clock-fit outlier rejection, mirroring the recorder-side fit.
    clock_outlier_sigma: float = 6.0
    clock_outlier_floor_ns: float = 2_000_000.0


@dataclass(frozen=True)
class FrameBarcode:
    """One decoded metadata barcode: a frame's clock anchor and packet samples."""

    video_index: int
    log_time_ns: int
    exposure_start_us: int
    exposure_end_us: int
    sample_timestamps_us: tuple[int, ...]
    sample_accel_mg: tuple[tuple[float, float, float], ...]

    @property
    def first_timestamp_us(self) -> int:
        return self.sample_timestamps_us[0]

    def spacing_us(self) -> float:
        return (self.sample_timestamps_us[-1] - self.sample_timestamps_us[0]) / (
            len(self.sample_timestamps_us) - 1
        )


def barcode_from_frame(
    video_index: int, log_time_ns: int, barcode: ImuFrame | None
) -> FrameBarcode | None:
    """Convert one decoded barcode into a :class:`FrameBarcode`, or ``None``."""
    if barcode is None or not barcode.samples or barcode.exposure_end_us <= 0:
        return None
    return FrameBarcode(
        video_index=video_index,
        log_time_ns=log_time_ns,
        exposure_start_us=barcode.exposure_start_us,
        exposure_end_us=barcode.exposure_end_us,
        sample_timestamps_us=tuple(sample.timestamp_us for sample in barcode.samples),
        sample_accel_mg=tuple(
            (sample.ax_mg, sample.ay_mg, sample.az_mg) for sample in barcode.samples
        ),
    )


@dataclass(frozen=True)
class ImuRetimingReport:
    """Serializable account of what the retimer decided and why."""

    algorithm_version: str
    decision: str
    reason: str
    frames_observed: int
    barcodes_decoded: int
    frames_without_barcode: int
    grid_spacing_us: float
    grid_anchor_offset_us: float
    frame_period_us: float
    frames_per_second: float
    messages_total: int
    messages_retimed: int
    packets_extrapolated: int
    duplicates_dropped: int
    clock_scale_ns_per_us: float
    clock_residual_p95_ms: float
    clock_inliers: int

    def as_dict(self) -> dict[str, object]:
        return {
            "algorithm_version": self.algorithm_version,
            "decision": self.decision,
            "reason": self.reason,
            "frames_observed": self.frames_observed,
            "barcodes_decoded": self.barcodes_decoded,
            "frames_without_barcode": self.frames_without_barcode,
            "grid_spacing_us": round(self.grid_spacing_us, 3),
            "grid_anchor_offset_us": round(self.grid_anchor_offset_us, 3),
            "frame_period_us": round(self.frame_period_us, 3),
            "frames_per_second": round(self.frames_per_second, 3),
            "messages_total": self.messages_total,
            "messages_retimed": self.messages_retimed,
            "packets_extrapolated": self.packets_extrapolated,
            "duplicates_dropped": self.duplicates_dropped,
            "clock_scale_ns_per_us": round(self.clock_scale_ns_per_us, 6),
            "clock_residual_p95_ms": round(self.clock_residual_p95_ms, 3),
            "clock_inliers": self.clock_inliers,
        }


@dataclass(frozen=True)
class ImuRetimingPlan:
    """Per-message host timestamps, or the reason the stream was left alone."""

    report: ImuRetimingReport
    #: Host log time per source IMU message; ``None`` drops a repeated sample.
    timestamps_ns: tuple[int | None, ...] = ()

    @property
    def applied(self) -> bool:
        return self.report.decision == DECISION_APPLIED

    @property
    def dropped(self) -> int:
        return sum(1 for value in self.timestamps_ns if value is None)


@dataclass(frozen=True)
class _GridFit:
    spacing_us: float = 0.0
    anchor_offset_us: float = 0.0
    frame_period_us: float = 0.0
    valid: bool = False
    reason: str = "not_fitted"


@dataclass(frozen=True)
class _ClockFit:
    scale_ns_per_us: float
    origin_ns: int
    origin_us: int
    residual_p95_ms: float
    inliers: int

    def to_host_ns(self, device_us: int) -> int:
        return self.origin_ns + round((device_us - self.origin_us) * self.scale_ns_per_us)


class ImuRetimingCalibrator:
    """Collect one recording's barcodes and IMU stream, then decide on a plan.

    Barcodes come from the decoded video frames; messages come from the source
    IMU topic in the order the conversion pass will emit them. Both are indexed
    by their position in that stream, which is what makes the alignment exact.
    """

    def __init__(self, config: ImuRetimingConfig | None = None) -> None:
        self.config = config or ImuRetimingConfig()
        self._barcodes: list[FrameBarcode] = []
        self._messages: list[tuple[float, float, float]] = []
        self._frames_observed = 0
        self._plan: ImuRetimingPlan | None = None

    @property
    def barcodes(self) -> list[FrameBarcode]:
        return self._barcodes

    @property
    def frames_observed(self) -> int:
        return self._frames_observed

    @property
    def messages_observed(self) -> int:
        return len(self._messages)

    def observe_frame(self, video_index: int, barcode: FrameBarcode | None) -> None:
        """Record one video message: its index, and its barcode when it decoded."""
        self._frames_observed = max(self._frames_observed, video_index + 1)
        if barcode is not None:
            self._barcodes.append(barcode)

    def observe_message(self, accel_mg: tuple[float, float, float]) -> None:
        """Record one source IMU message's acceleration, in emission order."""
        self._messages.append(accel_mg)

    def resolve(self) -> ImuRetimingPlan:
        """Return the retiming plan, or a skip decision that keeps source times."""
        if self._plan is not None:
            return self._plan
        grid = self._fit_grid()
        total = len(self._messages)
        if not grid.valid:
            self._plan = self._skip(grid.reason, grid, total)
            return self._plan
        if total < self.config.packet_size * self.config.packets_per_frame:
            self._plan = self._skip("insufficient_messages", grid, total)
            return self._plan
        clock = self._fit_clock()
        if clock is None:
            self._plan = self._skip("clock_fit_failed", grid, total)
            return self._plan
        grid_timestamps, extrapolated, reason = self._build_grid_timestamps(grid)
        if reason is not None:
            self._plan = self._skip(reason, grid, total)
            return self._plan

        timestamps: list[int | None] = []
        previous_us: int | None = None
        duplicates = 0
        # Every packet opens with a repeat of the previous packet's last sample.
        # The device timestamps carry microsecond rounding, so a repeat can land
        # one microsecond off its predecessor; anything closer than half a grid
        # step is a repeat, because the real grid never gets that close.
        repeat_limit_us = grid.spacing_us / 2
        for device_us in grid_timestamps:
            if device_us is None:
                timestamps.append(None)
                continue
            if previous_us is not None and (device_us - previous_us) < repeat_limit_us:
                duplicates += 1
                timestamps.append(None)
                continue
            previous_us = device_us
            timestamps.append(clock.to_host_ns(device_us))

        self._plan = ImuRetimingPlan(
            report=ImuRetimingReport(
                algorithm_version=ALGORITHM_VERSION,
                decision=DECISION_APPLIED,
                reason="grid_validated",
                frames_observed=self._frames_observed,
                barcodes_decoded=len(self._barcodes),
                frames_without_barcode=self._frames_observed - len(self._barcodes),
                grid_spacing_us=grid.spacing_us,
                grid_anchor_offset_us=grid.anchor_offset_us,
                frame_period_us=grid.frame_period_us,
                frames_per_second=1e6 / grid.frame_period_us,
                messages_total=total,
                messages_retimed=sum(1 for value in timestamps if value is not None),
                packets_extrapolated=extrapolated,
                duplicates_dropped=duplicates,
                clock_scale_ns_per_us=clock.scale_ns_per_us,
                clock_residual_p95_ms=clock.residual_p95_ms,
                clock_inliers=clock.inliers,
            ),
            timestamps_ns=tuple(timestamps),
        )
        return self._plan

    def _skip(self, reason: str, grid: _GridFit, total: int) -> ImuRetimingPlan:
        return ImuRetimingPlan(
            report=ImuRetimingReport(
                algorithm_version=ALGORITHM_VERSION,
                decision=DECISION_SKIPPED,
                reason=reason,
                frames_observed=self._frames_observed,
                barcodes_decoded=len(self._barcodes),
                frames_without_barcode=self._frames_observed - len(self._barcodes),
                grid_spacing_us=grid.spacing_us,
                grid_anchor_offset_us=grid.anchor_offset_us,
                frame_period_us=grid.frame_period_us,
                frames_per_second=1e6 / grid.frame_period_us if grid.frame_period_us else 0.0,
                messages_total=total,
                messages_retimed=0,
                packets_extrapolated=0,
                duplicates_dropped=0,
                clock_scale_ns_per_us=0.0,
                clock_residual_p95_ms=0.0,
                clock_inliers=0,
            ),
        )

    def _fit_grid(self) -> _GridFit:
        """Measure the grid constants and require them to hold on every barcode."""
        config = self.config
        usable = [
            barcode for barcode in self._barcodes
            if len(barcode.sample_timestamps_us) == config.packet_size
        ]
        if len(usable) < config.min_sampled_frames:
            return _GridFit(reason="insufficient_barcodes")

        spacings = np.asarray([barcode.spacing_us() for barcode in usable])
        spacing = float(np.median(spacings))
        if spacing <= 0 or float(np.max(np.abs(spacings - spacing))) > config.spacing_tolerance_us:
            return _GridFit(reason="spacing_not_constant")
        anchors = np.asarray(
            [barcode.first_timestamp_us - barcode.exposure_end_us for barcode in usable],
            dtype=np.float64,
        )
        anchor = float(np.median(anchors))
        if np.max(np.abs(anchors - anchor)) > config.anchor_tolerance_us:
            return _GridFit(reason="anchor_not_constant")

        frame_period = spacing * config.packets_per_frame * (config.packet_size - 1)
        ordered = sorted(usable, key=lambda barcode: barcode.video_index)
        stride_tolerance = max(4.0, spacing * 0.05)
        for previous, current in zip(ordered, ordered[1:]):
            frame_gap = current.video_index - previous.video_index
            if frame_gap <= 0:
                continue
            measured = (current.first_timestamp_us - previous.first_timestamp_us) / frame_gap
            if abs(measured - frame_period) > stride_tolerance:
                return _GridFit(reason="frame_stride_mismatch")
        return _GridFit(spacing_us=spacing, anchor_offset_us=anchor,
                        frame_period_us=frame_period, valid=True)

    def _build_grid_timestamps(
        self, grid: _GridFit
    ) -> tuple[list[int | None], int, str | None]:
        """Assign every source message a grid timestamp, verifying every barcode."""
        config = self.config
        messages = self._messages
        packet_size = config.packet_size
        packet_count = len(messages) // packet_size
        messages_per_frame = packet_size * config.packets_per_frame
        grid_timestamps: list[int | None] = [None] * len(messages)

        barcodes = sorted(self._barcodes, key=lambda barcode: barcode.video_index)
        anchor_barcode: FrameBarcode | None = None
        anchor_packets: int | None = None
        for barcode in barcodes:
            if len(barcode.sample_timestamps_us) != packet_size:
                continue
            expected = barcode.video_index * config.packets_per_frame
            found = None
            for offset in range(0, config.search_window_packets + 1):
                for candidate in ((expected + offset,) if offset == 0
                                  else (expected + offset, expected - offset)):
                    if candidate < 0 or candidate >= packet_count:
                        continue
                    if self._packet_matches(self._packet(messages, candidate), barcode):
                        found = candidate
                        break
                if found is not None:
                    break
            if found is None:
                return grid_timestamps, 0, "packet_alignment_failed"
            anchor_barcode = barcode
            anchor_packets = found - barcode.video_index * config.packets_per_frame
            break
        if anchor_barcode is None or anchor_packets is None:
            return grid_timestamps, 0, "no_usable_barcode"

        # Every decoded barcode is checked against its own anchor: the packet
        # values must match the source stream and the sample timestamps must sit
        # on the grid that the anchor implies.
        verified_anchors: dict[int, int] = {}
        for barcode in barcodes:
            expected_packet = barcode.video_index * config.packets_per_frame + anchor_packets
            if not 0 <= expected_packet < packet_count:
                continue
            if not self._packet_matches(self._packet(messages, expected_packet), barcode):
                return grid_timestamps, 0, "packet_alignment_drifted"
            expected_timestamps = np.asarray([
                barcode.first_timestamp_us
                + (position - position // config.packet_size) * grid.spacing_us
                for position in range(packet_size)
            ], dtype=np.int64)
            measured = np.asarray(barcode.sample_timestamps_us, dtype=np.int64)
            if int(np.max(np.abs(measured - expected_timestamps))) > config.timestamp_tolerance_us:
                return grid_timestamps, 0, "barcode_grid_mismatch"
            verified_anchors[barcode.video_index] = barcode.first_timestamp_us
        if len(verified_anchors) < config.min_sampled_frames:
            return grid_timestamps, 0, "insufficient_verified_frames"

        # Frames whose barcode decoded anchor themselves; the rest are placed on
        # the same rigid grid relative to the nearest verified frame.
        reference_video = min(verified_anchors)
        reference_us = verified_anchors[reference_video]

        def frame_start_us(video_index: int) -> float:
            anchor = verified_anchors.get(video_index)
            if anchor is not None:
                return float(anchor)
            return reference_us + grid.frame_period_us * (video_index - reference_video)

        verified_frames = set(verified_anchors)
        last_packet = packet_count - 1
        extrapolated = 0
        # The grid is rigid, so frames whose barcode could not be decoded - and
        # frames that precede the first decodable one - are placed by stringing
        # the same frame period out from the anchor frame.
        for video_index in range(0, self._frames_observed):
            base_packet = video_index * config.packets_per_frame + anchor_packets
            if base_packet < 0:
                continue
            if base_packet > last_packet:
                break
            if video_index not in verified_frames:
                extrapolated += 1
            start_us = frame_start_us(video_index)
            for position in range(messages_per_frame):
                packet_offset, in_packet = divmod(position, packet_size)
                packet = base_packet + packet_offset
                if packet > last_packet:
                    break
                index = packet * packet_size + in_packet
                offset = position - packet_offset
                grid_timestamps[index] = int(round(start_us + offset * grid.spacing_us))
        self._fill_grid_gaps(grid_timestamps, grid.spacing_us)
        return grid_timestamps, extrapolated, None

    @staticmethod
    def _fill_grid_gaps(timestamps: list[int | None], spacing_us: float) -> None:
        """Continue the grid over any message a whole packet could not cover.

        A recording does not always contain a whole number of packets, and the
        tail still sits on the same rigid grid, so it is filled forwards (or
        backwards when the stream opens mid-packet) rather than dropped.
        """
        step = int(round(spacing_us))
        last: int | None = None
        for index, value in enumerate(timestamps):
            if value is not None:
                last = value
            elif last is not None:
                last = last + step
                timestamps[index] = last
        following: int | None = None
        for index in range(len(timestamps) - 1, -1, -1):
            value = timestamps[index]
            if value is not None:
                following = value
            elif following is not None:
                following = following - step
                timestamps[index] = following

    def _packet(self, messages: list[tuple[float, float, float]], packet: int) -> np.ndarray:
        size = self.config.packet_size
        return np.asarray(messages[packet * size:(packet + 1) * size], dtype=np.float64)

    def _packet_matches(self, packet: np.ndarray, barcode: FrameBarcode) -> bool:
        wanted = np.asarray(barcode.sample_accel_mg, dtype=np.float64)
        if packet.shape != wanted.shape or packet.size == 0:
            return False
        return bool(np.max(np.abs(packet - wanted)) <= self.config.sample_tolerance_mg)

    def _fit_clock(self) -> _ClockFit | None:
        """Fit device microseconds onto the recording's own host timeline."""
        config = self.config
        barcodes = [barcode for barcode in self._barcodes if barcode.log_time_ns > 0]
        if len(barcodes) < config.min_sampled_frames:
            return None
        device = np.asarray([barcode.exposure_end_us for barcode in barcodes], dtype=np.float64)
        host = np.asarray([barcode.log_time_ns for barcode in barcodes], dtype=np.float64)
        origin_us = int(device[0])
        origin_ns = int(host[0])
        device = device - origin_us
        host = host - origin_ns
        if device[-1] - device[0] < 1_000.0:
            return None

        mask = np.ones(device.shape, dtype=bool)
        scale = 0.0
        residuals = np.zeros_like(device)
        for _ in range(5):
            if np.count_nonzero(mask) < 3:
                return None
            selected_device = device[mask]
            selected_host = host[mask]
            device_center = float(selected_device.mean())
            host_center = float(selected_host.mean())
            centred = selected_device - device_center
            denominator = float(np.dot(centred, centred))
            if denominator <= 0:
                return None
            scale = float(np.dot(centred, selected_host - host_center) / denominator)
            intercept = host_center - device_center * scale
            residuals = host - (intercept + device * scale)
            median = float(np.median(residuals))
            mad = float(np.median(np.abs(residuals - median)))
            threshold = max(config.clock_outlier_sigma * 1.4826 * mad,
                            config.clock_outlier_floor_ns)
            updated = np.abs(residuals - median) <= threshold
            if np.array_equal(updated, mask):
                break
            mask = updated
        if not config.min_clock_scale <= scale <= config.max_clock_scale:
            return None
        return _ClockFit(
            scale_ns_per_us=scale,
            origin_ns=int(round(origin_ns + intercept)),
            origin_us=origin_us,
            residual_p95_ms=float(np.percentile(np.abs(residuals[mask]), 95) / 1e6),
            inliers=int(np.count_nonzero(mask)),
        )
