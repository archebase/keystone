# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Regression tests for E2 capture alignment.

The device can lose frames while a camera pipeline spins up, so the two
cameras do not necessarily hold the same number of frames nor does frame N of
one camera share a capture instant with frame N of the other. The converter
used to pair by frame index and to force the container's nominal frame rate,
which both misaligned the stereo pairs and aborted the job outright. These
tests pin the timestamp-based behaviour that replaces it.
"""

import csv
import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))


def has_module(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except ModuleNotFoundError:
        return False


FRAME_STEP_NS = 33_334_000
T0 = 1_789_027_737_957_492_662


def capture_with_startup_gaps() -> tuple[list[int], list[int]]:
    """Timestamps shaped like a real capture: each camera stalls once at start."""
    left = [T0] + [T0 + 233_331_000 + i * FRAME_STEP_NS for i in range(350)]
    right_first = T0 + 500_000
    right = [right_first] + [
        right_first + 66_649_000 + i * FRAME_STEP_NS for i in range(355)
    ]
    return left, right


@unittest.skipUnless(
    has_module("numpy")
    and has_module("google.protobuf")
    and has_module("mcap")
    and has_module("rosbags"),
    "full E2 converter dependencies are available in the Job image",
)
class E2CaptureAlignmentTest(unittest.TestCase):
    def setUp(self) -> None:
        from e2_converter import (  # noqa: PLC0415 - heavy module, imported lazily
            _au_is_keyframe,
            _build_calibration,
            _camera_timestamps,
            _csv_rows,
            _dropped_frame_filter,
            _imu_rows,
            _pair_tolerance_ns,
            _plan_video_pairs,
            _recorded_exposure_timestamps,
            _validate_time_bases,
            _validate_timestamp_ceiling,
        )

        self.au_is_keyframe = _au_is_keyframe
        self.build_calibration = _build_calibration
        self.camera_timestamps = _camera_timestamps
        self.dropped_frame_filter = _dropped_frame_filter
        self.csv_rows = _csv_rows
        self.imu_rows = _imu_rows
        self.pair_tolerance_ns = _pair_tolerance_ns
        self.plan_video_pairs = _plan_video_pairs
        self.recorded_exposure_timestamps = _recorded_exposure_timestamps
        self.validate_time_bases = _validate_time_bases
        self.validate_timestamp_ceiling = _validate_timestamp_ceiling

    def test_time_bases_must_overlap(self) -> None:
        # One clock: the IMU starts just before the video and ends just after.
        self.validate_time_bases(1_000, 2_000, 900, 2_100)
        # Two clocks: the camera is in 2026, the IMU in a 1970-era session.
        with self.assertRaises(RuntimeError):
            self.validate_time_bases(
                1_789_000_000_000_000_000, 1_789_000_060_000_000_000,
                10_600_000_000_000, 10_612_000_000_000,
            )

    def test_timestamp_ceiling_rejects_a_clock_beyond_2040(self) -> None:
        self.validate_timestamp_ceiling(camera=[1_789_000_000_000_000_000])
        # ROS stores Time.sec in an int32, so the CDR writer cannot represent this.
        with self.assertRaises(RuntimeError):
            self.validate_timestamp_ceiling(camera=[3_578_000_000_000_000_000])

    def test_camera_frame_times_require_a_metainfo(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            video = Path(tmp) / "Camera0" / "video.mp4"
            video.parent.mkdir(parents=True)
            video.write_bytes(b"")
            with self.assertRaises(RuntimeError):
                self.camera_timestamps(video)

    def test_calibration_carries_the_camera_to_imu_time_offsets(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            camera = {
                "width": 1600, "height": 1200,
                "intrinsics": {
                    "focalX": 485.0, "focalY": 485.0, "centerX": 800.0, "centerY": 600.0,
                    "radialDistortion": [0.0, 0.0, 0.0, 0.0],
                },
                "extrinsics": {"position": [0.0, 0.0, 0.0], "rotation": [0.0, 0.0, 0.0, 1.0]},
            }
            for eye in ("Camera0", "Camera1"):
                (root / eye).mkdir(parents=True)
                (root / eye / "camera_params.json").write_text(
                    json.dumps({"cameras": [camera]})
                )
            (root / "Sensors").mkdir()
            (root / "Sensors" / "imu_calibration.json").write_text(json.dumps({
                "imu": {
                    "time_alignment_s": {
                        "imu_to_pose": 0.00168016471,
                        "accel": 0.0,
                        "cameras": {"rgb-left": 0.00168016471, "rgb-right": 0.00162890565},
                    },
                },
                "noise": {
                    "accel_noise_std_mps2": [0.02] * 3,
                    "gyro_noise_std_rads": [0.0016] * 3,
                    "accel_bias_std_mps2": [0.003] * 3,
                    "gyro_bias_std_rads": [0.0003] * 3,
                },
            }))
            calibration = self.build_calibration(root)
            offsets = {
                entry["from_clock"]: entry
                for entry in calibration["temporal_extrinsics"]
            }
            self.assertEqual(offsets["cam0"]["to_clock"], "imu0")
            self.assertEqual(offsets["cam0"]["convention"], "t_imu = t_camera + offset_seconds")
            self.assertAlmostEqual(offsets["cam0"]["offset_seconds"], 0.00168016471)
            self.assertAlmostEqual(offsets["cam1"]["offset_seconds"], 0.00162890565)

    def test_au_is_keyframe_detects_idr_slice(self) -> None:
        # Annex-B access units start with an AUD (type 9); an IDR slice is 5.
        aud = b"\x00\x00\x00\x01\x09\xf0"
        idr = b"\x00\x00\x00\x01\x65\x88"
        p_frame = b"\x00\x00\x00\x01\x41\x9a"
        self.assertTrue(self.au_is_keyframe(aud + b"\x00\x00\x00\x01\x67\x00" + idr))
        self.assertFalse(self.au_is_keyframe(aud + b"\x00\x00\x01\x41\x9a"))
        self.assertFalse(self.au_is_keyframe(p_frame))
        self.assertFalse(self.au_is_keyframe(b""))

    def test_dropped_frame_filter_is_none_when_nothing_is_dropped(self) -> None:
        self.assertIsNone(self.dropped_frame_filter([0, 1, 2], 3))

    def test_dropped_frame_filter_drops_head_gaps_and_tail(self) -> None:
        # Keep frames 2 and 5 out of 7; the head, the interior gap and the tail
        # must all be dropped, with the commas ffmpeg needs escaped.
        expression = self.dropped_frame_filter([2, 5], 7)
        self.assertEqual(
            expression,
            "not(between(n\\,0\\,1)+between(n\\,3\\,4)+between(n\\,6\\,6))",
        )

    def test_dropped_frame_filter_handles_a_single_kept_run(self) -> None:
        self.assertEqual(
            self.dropped_frame_filter([1, 2], 5),
            "not(between(n\\,0\\,0)+between(n\\,3\\,4))",
        )

    def test_pairing_tolerance_is_half_a_frame_period(self) -> None:
        from fractions import Fraction

        self.assertEqual(self.pair_tolerance_ns(Fraction(30, 1)), 16_666_666)
        # Never below 5 ms even for an implausible container rate.
        self.assertEqual(self.pair_tolerance_ns(Fraction(0)), 5_000_000)

    def test_index_pairing_would_misalign_but_time_pairing_does_not(self) -> None:
        left, right = capture_with_startup_gaps()
        tolerance = self.pair_tolerance_ns(30)
        pairs, unpaired_left, unpaired_right, max_offset = self.plan_video_pairs(
            left, right, tolerance
        )

        self.assertEqual(len(pairs), 351)
        self.assertEqual(unpaired_left, 0)
        self.assertEqual(unpaired_right, 5)
        self.assertLess(max_offset, 2_000_000)

        # The old index pairing compares different instants: the first frame of
        # each camera lines up, every later one is off by the startup gap.
        self.assertLess(abs(left[0] - right[0]), 2_000_000)
        self.assertGreater(abs(left[1] - right[1]), 100_000_000)
        # Time pairing keeps the pairing strictly monotonic in both streams.
        left_indices = [index for index, _ in pairs]
        right_indices = [index for _, index in pairs]
        self.assertEqual(left_indices, sorted(set(left_indices)))
        self.assertEqual(right_indices, sorted(set(right_indices)))

    def test_frames_without_a_partner_are_not_paired(self) -> None:
        left = [0, 1_000_000_000]
        right = [500_000, 5_000_000_000]
        pairs, unpaired_left, unpaired_right, _ = self.plan_video_pairs(
            left, right, 5_000_000
        )
        self.assertEqual(pairs, [(0, 0)])
        self.assertEqual(unpaired_left, 1)
        self.assertEqual(unpaired_right, 1)

    def test_no_pairs_when_only_far_apart_frames_exist(self) -> None:
        pairs, unpaired_left, unpaired_right, _ = self.plan_video_pairs(
            [0, 1_000_000], [50_000_000, 60_000_000], 5_000_000
        )
        self.assertEqual(pairs, [])
        self.assertEqual((unpaired_left, unpaired_right), (2, 2))

    def test_csv_rows_accepts_both_timestamp_columns(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            legacy = root / "legacy.csv"
            utc = root / "utc.csv"
            legacy.write_text("ts_ns,ax,ay,az\n1,1.0,2.0,3.0\n2,4.0,5.0,6.0\n")
            utc.write_text(
                "ts_utc_ns,ax,ay,az\n1,1.0,2.0,3.0\n2,4.0,5.0,6.0\n"
            )
            expected = [(1, (1.0, 2.0, 3.0)), (2, (4.0, 5.0, 6.0))]
            self.assertEqual(list(self.csv_rows(legacy)), expected)
            self.assertEqual(list(self.csv_rows(utc)), expected)

    def test_csv_rows_rejects_a_missing_timestamp_column(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "accel.csv"
            path.write_text("time,ax,ay,az\n1,1.0,2.0,3.0\n")
            with self.assertRaises(RuntimeError):
                list(self.csv_rows(path))

    def write_sensors(
        self,
        root: Path,
        offset_ns: int,
        rows: int,
        stamp: str = "ts_utc_ns",
        drop_gyro_index: int | None = None,
    ) -> None:
        sensors = root / "Sensors"
        sensors.mkdir(parents=True, exist_ok=True)
        step = 1_250_400
        for name, columns in (("accel.csv", "ax,ay,az"), ("gyro.csv", "gx,gy,gz")):
            shift = 0 if name.startswith("accel") else offset_ns
            with (sensors / name).open("w", newline="") as stream:
                writer = csv.writer(stream)
                writer.writerow([stamp, *columns.split(",")])
                for index in range(rows):
                    if index == drop_gyro_index and name.startswith("gyro"):
                        continue
                    writer.writerow(
                        [T0 + shift + index * step, 0.1, 0.2, 0.3]
                    )

    def test_imu_merge_pairs_independently_sampled_sensors(self) -> None:
        # Newer firmware samples accel and gyro a few hundred microseconds
        # apart, so exact timestamp equality discarded nearly every sample.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.write_sensors(root, offset_ns=303_691, rows=2000)
            merged = list(self.imu_rows(root))
            # The skew is smaller than half a sample period, so every sample
            # finds its partner.
            self.assertEqual(len(merged), 2000)

    def test_imu_merge_drops_a_sample_with_no_partner(self) -> None:
        # A gyro sample missing mid-stream leaves the matching accel sample a
        # full period from its nearest partner: past half a period the merge
        # refuses to guess rather than pairing the wrong sample.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.write_sensors(root, offset_ns=0, rows=100, drop_gyro_index=50)
            merged = list(self.imu_rows(root))
            self.assertEqual(len(merged), 99)

    def test_imu_merge_still_handles_legacy_firmware(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.write_sensors(root, offset_ns=0, rows=500, stamp="ts_ns")
            self.assertEqual(len(list(self.imu_rows(root))), 500)

    def test_recorded_exposure_timestamps_reads_both_columns(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            for column in ("exposure_start_utc_ns", "exposure_start_ns"):
                camera = Path(tmp) / column / "Camera0"
                camera.mkdir(parents=True)
                (camera / "metainfo.csv").write_text(
                    f"pts_us(exp_end),{column},exposure_duration_ns,gain\n1,100,3,1\n2,200,3,1\n"
                )
                self.assertEqual(
                    self.recorded_exposure_timestamps(camera / "video.mp4"),
                    [100, 200],
                )

    def test_recorded_exposure_timestamps_returns_none_without_metainfo(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(
                self.recorded_exposure_timestamps(Path(tmp) / "Camera0/video.mp4")
            )


if __name__ == "__main__":
    unittest.main()
