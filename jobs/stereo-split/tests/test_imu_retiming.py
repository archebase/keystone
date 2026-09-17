# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

from pathlib import Path
import sys
import unittest

import numpy as np


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from imu_retiming import (  # noqa: E402
    DECISION_APPLIED,
    DECISION_SKIPPED,
    FrameBarcode,
    ImuRetimingCalibrator,
    ImuRetimingConfig,
)


SPACING_US = 1664
PACKET_SIZE = 11
PACKETS_PER_FRAME = 2
FRAME_PERIOD_US = SPACING_US * (PACKET_SIZE - 1) * PACKETS_PER_FRAME
ANCHOR_OFFSET_US = -16717
BASE_TIMESTAMP_US = 3_000_000_000


def sample_values(index: int) -> tuple[float, float, float]:
    return (120.0 + index * 0.35, -40.0 + index * 0.15, -900.0 + index * 0.05)


def device_timestamp_us(sample_index: int) -> int:
    """Device timestamp of one unique sample on the rigid grid."""
    return BASE_TIMESTAMP_US + SPACING_US * (sample_index + 1)


def frame_barcode(video_index: int, first_sample_index: int) -> FrameBarcode:
    """Barcode of one frame: packet A, which opens with the previous packet's last sample."""
    timestamps = tuple(device_timestamp_us(first_sample_index - 1 + position)
                       for position in range(PACKET_SIZE))
    values = tuple(sample_values(first_sample_index - 1 + position)
                   for position in range(PACKET_SIZE))
    exposure_end = timestamps[0] - ANCHOR_OFFSET_US
    return FrameBarcode(
        video_index=video_index,
        log_time_ns=exposure_end * 1000,
        exposure_start_us=exposure_end - 7495,
        exposure_end_us=exposure_end,
        sample_timestamps_us=timestamps,
        sample_accel_mg=values,
    )


def build_stream(frames: int) -> tuple[ImuRetimingCalibrator, list[tuple[float, float, float]]]:
    """Feed a calibrator with a rigid-grid barcode and a matching IMU stream.

    Each frame carries 20 new samples; packet A is the previous frame's last
    sample followed by ten new ones, and packet B is packet A's last sample
    followed by ten more, so every packet opens with a repeat.
    """
    calibrator = ImuRetimingCalibrator(ImuRetimingConfig())
    messages: list[tuple[float, float, float]] = []
    for video_index in range(frames):
        first_sample_index = 20 * video_index
        calibrator.observe_frame(video_index, frame_barcode(video_index, first_sample_index))
        for position in range(PACKET_SIZE * PACKETS_PER_FRAME):
            index = first_sample_index - 1 + position - (1 if position >= PACKET_SIZE else 0)
            value = sample_values(index)
            messages.append(value)
            calibrator.observe_message(value)
    return calibrator, messages


class ImuRetimingGridTest(unittest.TestCase):
    def test_retimes_the_whole_stream_onto_the_device_grid(self) -> None:
        calibrator, _ = build_stream(12)

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_APPLIED)
        self.assertEqual(plan.report.reason, "grid_validated")
        self.assertAlmostEqual(plan.report.grid_spacing_us, float(SPACING_US))
        self.assertAlmostEqual(plan.report.frame_period_us, float(FRAME_PERIOD_US))
        self.assertAlmostEqual(plan.report.frames_per_second, 1e6 / FRAME_PERIOD_US, places=3)
        self.assertEqual(plan.report.messages_total, 12 * 22)
        self.assertEqual(plan.report.duplicates_dropped, 12 * 2 - 1)
        self.assertEqual(plan.report.messages_retimed, 12 * 20 + 1)
        self.assertAlmostEqual(plan.report.clock_scale_ns_per_us, 1000.0, places=3)

    def test_retimed_timestamps_are_uniform_and_monotonic(self) -> None:
        calibrator, _ = build_stream(12)

        plan = calibrator.resolve()
        timestamps = [value for value in plan.timestamps_ns if value is not None]
        steps = np.diff(timestamps)

        self.assertTrue(all(step == SPACING_US * 1000 for step in steps))
        self.assertTrue(all(later > earlier for earlier, later in zip(timestamps, timestamps[1:])))

    def test_repeats_are_the_only_messages_dropped(self) -> None:
        calibrator, messages = build_stream(8)

        plan = calibrator.resolve()

        kept = [message for message, value in zip(messages, plan.timestamps_ns) if value is not None]
        dropped = [message for message, value in zip(messages, plan.timestamps_ns) if value is None]
        self.assertEqual(len(kept) + len(dropped), len(messages))
        # A dropped message always repeats the value of its predecessor.
        for index, value in enumerate(plan.timestamps_ns):
            if value is None:
                self.assertEqual(messages[index], messages[index - 1])

    def test_a_repeat_that_rounds_one_microsecond_late_is_still_dropped(self) -> None:
        """Device timestamps round to whole microseconds, so repeats can drift."""
        calibrator, _ = build_stream(12)
        barcodes = calibrator.barcodes
        broken = barcodes[4]
        calibrator._barcodes[4] = FrameBarcode(
            video_index=broken.video_index,
            log_time_ns=broken.log_time_ns,
            exposure_start_us=broken.exposure_start_us,
            exposure_end_us=broken.exposure_end_us,
            sample_timestamps_us=tuple(value + 1 for value in broken.sample_timestamps_us),
            sample_accel_mg=broken.sample_accel_mg,
        )

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_APPLIED)
        self.assertEqual(plan.report.duplicates_dropped, 12 * 2 - 1)

    def test_trailing_messages_are_placed_on_the_grid(self) -> None:
        """A capture may not contain a whole number of packets."""
        calibrator, _ = build_stream(12)
        tail = [sample_values(500 + index) for index in range(5)]
        for value in tail:
            calibrator.observe_message(value)

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_APPLIED)
        self.assertEqual(len(plan.timestamps_ns), 12 * 22 + 5)
        timestamps = [value for value in plan.timestamps_ns if value is not None]
        self.assertTrue(all(step == SPACING_US * 1000 for step in np.diff(timestamps)))

    def test_frames_without_a_barcode_are_placed_on_the_grid(self) -> None:
        calibrator = ImuRetimingCalibrator(ImuRetimingConfig())
        for video_index in range(10):
            first_sample_index = 20 * video_index
            barcode = (None if video_index in (3, 4)
                       else frame_barcode(video_index, first_sample_index))
            calibrator.observe_frame(video_index, barcode)
            for position in range(PACKET_SIZE * PACKETS_PER_FRAME):
                index = first_sample_index - 1 + position - (1 if position >= PACKET_SIZE else 0)
                calibrator.observe_message(sample_values(index))

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_APPLIED)
        self.assertEqual(plan.report.frames_without_barcode, 2)
        self.assertEqual(plan.report.packets_extrapolated, 2)
        timestamps = [value for value in plan.timestamps_ns if value is not None]
        self.assertTrue(all(step == SPACING_US * 1000 for step in np.diff(timestamps)))


class ImuRetimingSkipTest(unittest.TestCase):
    def test_skips_without_enough_barcodes(self) -> None:
        calibrator, _ = build_stream(3)

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_SKIPPED)
        self.assertEqual(plan.report.reason, "insufficient_barcodes")
        self.assertEqual(plan.timestamps_ns, ())

    def test_skips_when_the_grid_spacing_is_not_constant(self) -> None:
        calibrator, _ = build_stream(12)
        barcodes = calibrator.barcodes
        broken = barcodes[5]
        calibrator._barcodes[5] = FrameBarcode(
            video_index=broken.video_index,
            log_time_ns=broken.log_time_ns,
            exposure_start_us=broken.exposure_start_us,
            exposure_end_us=broken.exposure_end_us,
            sample_timestamps_us=tuple(
                value + (900 if position == 0 else 0)
                for position, value in enumerate(broken.sample_timestamps_us)
            ),
            sample_accel_mg=broken.sample_accel_mg,
        )

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_SKIPPED)
        self.assertIn(plan.report.reason, ("spacing_not_constant", "frame_stride_mismatch"))

    def test_skips_when_the_exposure_anchor_moves(self) -> None:
        calibrator, _ = build_stream(12)
        barcodes = calibrator.barcodes
        broken = barcodes[6]
        calibrator._barcodes[6] = FrameBarcode(
            video_index=broken.video_index,
            log_time_ns=broken.log_time_ns,
            exposure_start_us=broken.exposure_start_us - 5_000,
            exposure_end_us=broken.exposure_end_us - 5_000,
            sample_timestamps_us=broken.sample_timestamps_us,
            sample_accel_mg=broken.sample_accel_mg,
        )

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_SKIPPED)
        self.assertEqual(plan.report.reason, "anchor_not_constant")

    def test_skips_when_the_source_packets_do_not_match_the_barcode(self) -> None:
        calibrator = ImuRetimingCalibrator(ImuRetimingConfig())
        for video_index in range(10):
            calibrator.observe_frame(video_index, frame_barcode(video_index, 20 * video_index))
            for _ in range(PACKET_SIZE * PACKETS_PER_FRAME):
                calibrator.observe_message((0.0, 0.0, 0.0))

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_SKIPPED)
        self.assertEqual(plan.report.reason, "packet_alignment_failed")

    def test_skips_when_the_clock_rate_is_implausible(self) -> None:
        calibrator, _ = build_stream(12)
        calibrator._barcodes = [
            FrameBarcode(
                video_index=barcode.video_index,
                log_time_ns=3_000_000_000_000 + barcode.video_index * FRAME_PERIOD_US * 2_000,
                exposure_start_us=barcode.exposure_start_us,
                exposure_end_us=barcode.exposure_end_us,
                sample_timestamps_us=barcode.sample_timestamps_us,
                sample_accel_mg=barcode.sample_accel_mg,
            )
            for barcode in calibrator.barcodes
        ]

        plan = calibrator.resolve()

        self.assertEqual(plan.report.decision, DECISION_SKIPPED)
        self.assertEqual(plan.report.reason, "clock_fit_failed")


if __name__ == "__main__":
    unittest.main()
