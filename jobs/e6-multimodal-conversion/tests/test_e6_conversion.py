# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Tests for the E6 multimodal converter.

The end-to-end case builds a real side-by-side stereo capture with ffmpeg and
runs the converter over it, so the crop, the HEVC decode, the H.264 re-encode
and the MCAP contract are all exercised together; the unit cases pin the pieces
that are easy to break silently (metainfo timestamps, IMU column names, the
stereo iterator).
"""
import csv
import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import e6_converter  # noqa: E402
from e6_converter import (  # noqa: E402
    EncodingReport,
    VideoFrame,
    _au_is_keyframe,
    _csv_rows,
    _frame_timestamps,
    _stereo_pair_frames,
    convert,
)

HAS_FFMPEG = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None
BASE_NS = 1_790_656_317_378_343_785


class FrameTimestampsTest(unittest.TestCase):
    def make_capture(self, root: Path, metainfo: str, frames: int) -> Path:
        video = root / "rgb.mp4"
        video.write_bytes(b"stub")
        (root / "rgb_metainfo.csv").write_text(metainfo)
        return video

    def test_prefers_the_middle_of_the_exposure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            video = self.make_capture(root, (
                "frame_index,frame_id,pts_us,exposure_start_utc_ns,exposure_duration_ns,"
                "gain,mid_exposure_utc_ns\n"
                f"0,1,0,{BASE_NS},4000,50,{BASE_NS + 2000}\n"
                f"1,2,33333,{BASE_NS + 33_333_333},4000,50,{BASE_NS + 33_335_333}\n"
            ), 2)
            with mock.patch.object(e6_converter, "_container_frame_count", return_value=2):
                timestamps, source = _frame_timestamps(root, "rgb", video)
            self.assertEqual(source, "mid_exposure_utc_ns")
            self.assertEqual(timestamps, [BASE_NS + 2000, BASE_NS + 33_335_333])

    def test_falls_back_to_half_the_exposure_window(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            video = self.make_capture(root, (
                "frame_index,exposure_start_utc_ns,exposure_duration_ns\n"
                f"0,{BASE_NS},4000\n"
            ), 1)
            with mock.patch.object(e6_converter, "_container_frame_count", return_value=1):
                timestamps, source = _frame_timestamps(root, "rgb", video)
            self.assertEqual(source, "exposure_start_utc_ns")
            self.assertEqual(timestamps, [BASE_NS + 2000])

    def test_rejects_a_metainfo_that_does_not_match_the_video(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            video = self.make_capture(root, (
                "frame_index,mid_exposure_utc_ns\n" f"0,{BASE_NS}\n"
            ), 2)
            with mock.patch.object(e6_converter, "_container_frame_count", return_value=2):
                with self.assertRaises(RuntimeError):
                    _frame_timestamps(root, "rgb", video)


class ImuCsvTest(unittest.TestCase):
    def test_reads_the_e6_timestamp_column(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "accel.csv"
            with path.open("w", newline="") as stream:
                writer = csv.writer(stream)
                writer.writerow(["timestamp_ns", "x", "y", "z"])
                writer.writerow([BASE_NS, "0.5", "-1.5", "9.8"])
            self.assertEqual(list(_csv_rows(path)), [(BASE_NS, (0.5, -1.5, 9.8))])


class StereoIteratorTest(unittest.TestCase):
    def test_emits_both_eyes_with_the_recorded_timestamp(self) -> None:
        timestamps = [BASE_NS, BASE_NS + 33_333_333]
        report = EncodingReport()

        def fake_video_frames(path, kept, stamps, warnings, crop=None):
            for index in kept:
                # 0x65 is an IDR slice, so the first frame must be reported as a keyframe.
                yield VideoFrame(stamps[index], b"\x00\x00\x00\x01\x65" + crop.encode(), index)

        with mock.patch.object(e6_converter, "_video_frames", side_effect=fake_video_frames) as patched:
            pairs = list(_stereo_pair_frames(Path("rgb.mp4"), timestamps, report))

        self.assertEqual([pair[0] for pair in pairs], timestamps)
        left, right = pairs[0][1], pairs[0][2]
        self.assertIn(e6_converter.LEFT_CROP.encode(), left.data)
        self.assertIn(e6_converter.RIGHT_CROP.encode(), right.data)
        self.assertTrue(_au_is_keyframe(left.data) and _au_is_keyframe(right.data))
        self.assertTrue(report.first_frame_keyframe["left"])
        self.assertTrue(report.first_frame_keyframe["right"])
        # The same source file is decoded once per eye.
        self.assertEqual([call.args[4] for call in patched.call_args_list],
                         [e6_converter.LEFT_CROP, e6_converter.RIGHT_CROP])

    def test_crop_filter_only_touches_the_expected_region(self) -> None:
        # The left crop keeps x=0 and the right crop starts at half the width.
        self.assertEqual(e6_converter.LEFT_CROP, "crop=iw/2:ih:0:0")
        self.assertEqual(e6_converter.RIGHT_CROP, "crop=iw/2:ih:iw/2:0")


@unittest.skipUnless(HAS_FFMPEG, "ffmpeg is required for the end-to-end conversion test")
class EndToEndConversionTest(unittest.TestCase):
    def build_capture(self, root: Path, frames: int = 5, width: int = 320, height: int = 120) -> None:
        # A side-by-side HEVC capture, exactly how the device writes rgb.mp4.
        subprocess.run([
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
            "-f", "lavfi", "-i", f"testsrc=size={width}x{height}:rate=10",
            "-frames:v", str(frames), "-c:v", "libx265", "-pix_fmt", "yuv420p",
            "-tag:v", "hvc1", "-x265-params", "log-level=error",
            str(root / "rgb.mp4"),
        ], check=True)
        with (root / "rgb_metainfo.csv").open("w", newline="") as stream:
            writer = csv.writer(stream)
            writer.writerow(["frame_index", "frame_id", "pts_us", "exposure_start_utc_ns",
                             "exposure_duration_ns", "gain", "mid_exposure_utc_ns"])
            for index in range(frames):
                start = BASE_NS + index * 100_000_000
                writer.writerow([index, index, index * 100_000, start, 4_000, 50, start + 2_000])
        for name, column in (("accel.csv", "x"), ("gyro.csv", "x")):
            with (root / name).open("w", newline="") as stream:
                writer = csv.writer(stream)
                writer.writerow(["timestamp_ns", "x", "y", "z"])
                for index in range(2 * frames):
                    writer.writerow([BASE_NS + index * 50_000_000, "0.1", "0.2", "9.8"])
        (root / "camera_params_rgb.json").write_text(json.dumps({
            "group": "rgb",
            "cameras": [
                {
                    "eye": eye, "width": width // 2, "height": height,
                    "intrinsics": {
                        "focalX": 100.0, "focalY": 101.0, "centerX": 80.0, "centerY": 60.0,
                        "radialDistortion": [1, 2, 3, 4, 5],
                    },
                    "extrinsics": {"position": [offset, 0.0, 0.0], "rotation": [0, 0, 0, 1]},
                }
                for eye, offset in (("left", 0.0), ("right", 0.1))
            ],
        }))
        (root / "imu_calibration.json").write_text(json.dumps({
            "device_uid": "test",
            "imu": {"time_alignment_s": {"cameras": {"rgb-left": -0.001, "rgb-right": -0.002}}},
            "noise": {
                "accel_noise_std_mps2": [0.02] * 3,
                "accel_bias_std_mps2": [0.05] * 3,
                "gyro_noise_std_rads": [0.0016] * 3,
                "gyro_bias_std_rads": [0.005] * 3,
            },
        }))

    def test_converts_a_side_by_side_capture(self) -> None:
        from mcap.reader import make_reader
        from google.protobuf import descriptor_pb2, descriptor_pool, message_factory, timestamp_pb2  # noqa: F401

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "capture"
            root.mkdir()
            out = Path(directory) / "out"
            self.build_capture(root)
            result = convert(root, out)

            stats = result["stats"]
            self.assertEqual(stats["left_video_frames"], 5)
            self.assertEqual(stats["right_video_frames"], 5)
            self.assertGreater(stats["imu_messages"], 0)
            self.assertTrue(stats["left_first_frame_keyframe"])
            self.assertTrue(stats["right_first_frame_keyframe"])
            self.assertEqual(stats["left_timestamp_source"], "mid_exposure_utc_ns")

            with (out / "output_bag.mcap").open("rb") as stream:
                summary = make_reader(stream).get_summary()
                schemas = {schema.id: schema for schema in summary.schemas.values()}
                channels = {channel.topic: channel for channel in summary.channels.values()}
                self.assertEqual(set(channels), {
                    "/camera/left/image/h264", "/camera/right/image/h264", "/imu/data",
                })
                self.assertEqual(schemas[channels["/camera/left/image/h264"].schema_id].name,
                                 "foxglove.CompressedVideo")
                self.assertEqual(schemas[channels["/imu/data"].schema_id].encoding, "ros2msg")
                self.assertEqual(channels["/imu/data"].message_encoding, "cdr")
                counts = {
                    topic: summary.statistics.channel_message_counts.get(channel.id, 0)
                    for topic, channel in channels.items()
                }
            self.assertEqual(counts["/camera/left/image/h264"], 5)
            self.assertEqual(counts["/camera/right/image/h264"], 5)

            # Both eyes decode as H.264 at half the side-by-side width.
            self.assertEqual(self.eye_width(out / "output_bag.mcap"), 160)

            calibration = json.loads((out / "calibration.json").read_text())
            self.assertEqual([camera["resolution"] for camera in calibration["cameras"]],
                             [[160, 120], [160, 120]])
            self.assertTrue((out / "metadata.yaml").is_file())

    def eye_width(self, mcap: Path) -> int:
        from mcap.reader import make_reader
        from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

        descriptor = descriptor_pb2.FileDescriptorProto(
            name="foxglove/CompressedVideo.proto", package="foxglove", syntax="proto3")
        message = descriptor.message_type.add()
        message.name = "CompressedVideo"
        for name, number, ftype in (("frame_id", 2, 9), ("data", 3, 12), ("format", 4, 9)):
            field = message.field.add()
            field.name = name
            field.number = number
            field.type = ftype
            field.label = 1
        pool = descriptor_pool.Default()
        pool.Add(descriptor)
        compressed_video = message_factory.GetMessageClass(
            pool.FindMessageTypeByName("foxglove.CompressedVideo"))

        with mcap.open("rb") as stream:
            for _schema, _channel, message in make_reader(stream).iter_messages(
                topics=["/camera/left/image/h264"]
            ):
                video = compressed_video()
                video.ParseFromString(message.data)
                with tempfile.NamedTemporaryFile(suffix=".h264") as handle:
                    handle.write(video.data)
                    handle.flush()
                    probe = subprocess.run(
                        ["ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height",
                         "-of", "csv=p=0", handle.name],
                        check=True, capture_output=True, text=True,
                    )
                codec, width, height = probe.stdout.strip().split(",")[:3]
                self.assertEqual(codec, "h264")
                return int(width)
        self.fail("the left video channel holds no message")


if __name__ == "__main__":
    unittest.main()
