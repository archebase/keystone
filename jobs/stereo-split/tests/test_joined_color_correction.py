# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""End-to-end checks for the joined H.264 colour-consistency integration."""

from __future__ import annotations

import re
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import cv2
from mcap.reader import make_reader
from mcap.writer import IndexType, Writer
import numpy as np
from rosbags.typesys import Stores, get_typestore


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from color_consistency import ColorConsistencyConfig  # noqa: E402
from convert_mcap_stereo_h264 import (  # noqa: E402
    CompressedVideo,
    ConverterConfig,
    FOXGLOVE_DESCRIPTOR_SET,
    FOXGLOVE_SCHEMA_NAME,
    StereoSplitH264Converter,
    _build_timestamp_repair_plan,
)
sys.path.remove(str(JOB_ROOT))


WIDTH = 1920
HEIGHT = 1200
METADATA_WIDTH = 160
FRAME_WIDTH = METADATA_WIDTH + WIDTH * 2
FRAME_COUNT = 4
RIGHT_GAINS = (.62, 1.38, 1.26)


def textured_eye(seed: int = 11) -> np.ndarray:
    """Build one eye-sized textured BGR frame with unsaturated levels."""
    rng = np.random.default_rng(seed)
    eye = np.zeros((HEIGHT, WIDTH, 3), dtype=np.float64)
    yy, xx = np.mgrid[0:HEIGHT, 0:WIDTH]
    eye[..., 0] = 70 + 80 * xx / WIDTH
    eye[..., 1] = 90 + 70 * yy / HEIGHT
    eye[..., 2] = 110 - 50 * xx / WIDTH
    for _ in range(700):
        center = (int(rng.integers(20, WIDTH - 20)), int(rng.integers(20, HEIGHT - 20)))
        level = float(rng.integers(20, 180))
        cv2.circle(
            eye,
            center,
            int(rng.integers(8, 26)),
            (level, min(180.0, level + 40), max(15.0, level - 50)),
            -1,
        )
    return cv2.GaussianBlur(np.clip(eye, 0, 185).astype(np.uint8), (0, 0), 1.2)


def gained(eye: np.ndarray) -> np.ndarray:
    """Apply a per-channel gain and a horizontal grade as the colour mismatch."""
    field = 1.0 + .3 * (.5 - np.linspace(0, 1, WIDTH)[None, :, None])
    values = eye.astype(np.float64) / 255.0 * np.asarray(RIGHT_GAINS)[None, None, :] * field
    return np.clip(np.rint(values * 255.0), 0, 255).astype(np.uint8)


def joined_frame(index: int, textured: bool = True) -> np.ndarray:
    """Compose one joined full-frame image from the two eyes."""
    if textured:
        left = textured_eye()
        faded = cv2.convertScaleAbs(left, alpha=1 - .01 * index, beta=0)
        right = gained(faded)
    else:
        left = np.full((HEIGHT, WIDTH, 3), 120, dtype=np.uint8)
        right = np.full((HEIGHT, WIDTH, 3), 140, dtype=np.uint8)
    frame = np.zeros((HEIGHT, FRAME_WIDTH, 3), dtype=np.uint8)
    frame[:, :METADATA_WIDTH] = 255
    frame[:, METADATA_WIDTH:METADATA_WIDTH + WIDTH] = left
    frame[:, METADATA_WIDTH + WIDTH:] = right
    return frame


def encode_access_units(frames: list[np.ndarray]) -> list[bytes]:
    encoded = subprocess.run(
        [
            "ffmpeg", "-hide_banner", "-loglevel", "error",
            "-f", "rawvideo", "-pixel_format", "bgr24",
            "-video_size", f"{FRAME_WIDTH}x{HEIGHT}", "-framerate", "60", "-i", "pipe:0",
            "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
            "-profile:v", "high", "-pix_fmt", "yuv420p", "-g", "1", "-bf", "0",
            "-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "pipe:1",
        ],
        input=b"".join(frame.tobytes() for frame in frames),
        capture_output=True,
        check=True,
    ).stdout
    starts = [match.start() for match in re.finditer(b"\\x00\\x00(?:\\x00)?\\x01\\x09", encoded)]
    units = [
        encoded[start: starts[index + 1] if index + 1 < len(starts) else len(encoded)]
        for index, start in enumerate(starts)
    ]
    if len(units) != len(frames):
        raise RuntimeError(f"fixture access unit mismatch: {len(units)} != {len(frames)}")
    return units


def decode_eye_topic(path: Path, topic: str) -> list[np.ndarray]:
    """Decode every message of one eye topic as a single H.264 stream.

    Only the first access unit carries SPS/PPS, so the frames have to be decoded
    in order by one process rather than one access unit at a time.
    """
    payload = bytearray()
    with path.open("rb") as stream:
        for _, _, message in make_reader(stream).iter_messages(topics=[topic]):
            payload.extend(CompressedVideo.FromString(message.data).data)
    decoded = subprocess.run(
        [
            "ffmpeg", "-hide_banner", "-loglevel", "error",
            "-f", "h264", "-i", "pipe:0",
            "-pix_fmt", "bgr24", "-f", "rawvideo", "pipe:1",
        ],
        input=bytes(payload),
        capture_output=True,
        check=True,
    ).stdout
    frame_bytes = WIDTH * HEIGHT * 3
    if len(decoded) % frame_bytes:
        raise RuntimeError(f"decoded stream size mismatch: {len(decoded)}")
    buffer = np.frombuffer(decoded, dtype=np.uint8)
    return [
        buffer[index * frame_bytes:(index + 1) * frame_bytes].reshape(HEIGHT, WIDTH, 3)
        for index in range(len(decoded) // frame_bytes)
    ]


def make_joined_source(path: Path, frames: list[np.ndarray]) -> None:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    messages = typestore.types
    units = encode_access_units(frames)
    imu_definition, _ = typestore.generate_msgdef("sensor_msgs/msg/Imu", ros_version=2)
    string_definition, _ = typestore.generate_msgdef("std_msgs/msg/String", ros_version=2)
    with path.open("wb") as stream:
        writer = Writer(
            stream,
            index_types=IndexType.ALL,
            repeat_channels=True,
            repeat_schemas=True,
            use_statistics=True,
            use_summary_offsets=True,
        )
        writer.start()
        image_schema = writer.register_schema(
            "sensor_msgs/msg/CompressedImage", "ros2msg",
            typestore.generate_msgdef(
                "sensor_msgs/msg/CompressedImage", ros_version=2
            )[0].encode(),
        )
        image_channel = writer.register_channel(
            "/decxin/rgb/compressed", "cdr", image_schema)
        imu_schema = writer.register_schema(
            "sensor_msgs/msg/Imu", "ros2msg", imu_definition.encode())
        imu_channel = writer.register_channel("/decxin/imu", "cdr", imu_schema)
        serial_schema = writer.register_schema(
            "std_msgs/msg/String", "ros2msg", string_definition.encode())
        serial_channel = writer.register_channel("/decxin/serial_number", "cdr", serial_schema)
        for index, unit in enumerate(units):
            timestamp = (index + 1) * 33_333_333
            stamp = messages["builtin_interfaces/msg/Time"](sec=index, nanosec=timestamp)
            image = messages["sensor_msgs/msg/CompressedImage"](
                header=messages["std_msgs/msg/Header"](stamp=stamp, frame_id="joined_camera"),
                format="h264",
                data=np.frombuffer(unit, dtype=np.uint8),
            )
            writer.add_message(
                image_channel, timestamp,
                bytes(typestore.serialize_cdr(image, "sensor_msgs/msg/CompressedImage")),
                timestamp, index,
            )
            imu = messages["sensor_msgs/msg/Imu"](
                header=messages["std_msgs/msg/Header"](stamp=stamp, frame_id="decxin_imu"),
                orientation=messages["geometry_msgs/msg/Quaternion"](x=0.0, y=0.0, z=0.0, w=1.0),
                orientation_covariance=np.zeros(9, dtype=np.float64),
                angular_velocity=messages["geometry_msgs/msg/Vector3"](x=0.0, y=0.0, z=0.0),
                angular_velocity_covariance=np.zeros(9, dtype=np.float64),
                linear_acceleration=messages["geometry_msgs/msg/Vector3"](x=0.0, y=0.0, z=9.8),
                linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
            )
            writer.add_message(
                imu_channel, timestamp + 1,
                bytes(typestore.serialize_cdr(imu, "sensor_msgs/msg/Imu")),
                timestamp + 1, index,
            )
            serial = messages["std_msgs/msg/String"](data="EP-000013-BD")
            writer.add_message(
                serial_channel, timestamp + 2,
                bytes(typestore.serialize_cdr(serial, "std_msgs/msg/String")),
                timestamp + 2, index,
            )
        writer.finish()


def test_color_config(spatial_grid: int = 0) -> ColorConsistencyConfig:
    return ColorConsistencyConfig(
        sample_step=1,
        match_scale=1.0,
        max_match_features=2000,
        gain_bins=4,
        spatial_grid=spatial_grid,
        min_gain_bin_samples=20,
        min_spatial_cell_samples=5,
        min_sampled_frames=FRAME_COUNT,
        min_matched_samples=100,
        train_validation_block_size=1,
    )


def eye_of(frame: np.ndarray, position: str) -> np.ndarray:
    if position == "left":
        return frame[:, METADATA_WIDTH:METADATA_WIDTH + WIDTH]
    return frame[:, METADATA_WIDTH + WIDTH:]


def decode_topic(path: Path, topic: str) -> list[np.ndarray]:
    """Decode an output eye topic; both stereo topics are eye-sized."""
    frames = decode_eye_topic(path, topic)
    if not frames:
        raise RuntimeError(f"no messages on {topic}")
    return frames


def mean_abs(first: np.ndarray, second: np.ndarray) -> float:
    return float(np.mean(np.abs(first.astype(np.float64) - second.astype(np.float64))))


def run_runner(source: Path, output_binding: Path, scratch: Path) -> subprocess.CompletedProcess[str]:
    """Run the published Job entrypoint the way Orbit invokes it."""
    command = [
        sys.executable, str(JOB_ROOT / "run_processing.py"),
        "--input", str(source),
        "--output-binding", str(output_binding),
        "--scratch", str(scratch),
        "--expected-source-size", str(source.stat().st_size),
        "--expected-source-checksum", hashlib.sha256(source.read_bytes()).hexdigest(),
        "--source-uri", "tos://test-bucket/raw/source.mcap",
        "--processor-image", "ghcr.io/archebase/stereo-split@sha256:test",
        "--kind", "stereo_split",
        "--generation", "1",
    ]
    return subprocess.run(command, cwd=JOB_ROOT, capture_output=True, text=True, check=False)


class JoinedColorCorrectionTest(unittest.TestCase):
    def convert(self, source: Path, output: Path, **kwargs):
        return StereoSplitH264Converter(
            ConverterConfig(), kwargs.pop("color_config", test_color_config())
        ).convert(source, output)

    def test_corrects_joined_h264_right_eye_and_keeps_left_eye(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames = [joined_frame(index) for index in range(FRAME_COUNT)]
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_joined_source(source, frames)

            stats = self.convert(source, output)

            self.assertEqual(stats.input_mode, "joined")
            self.assertEqual(stats.color_decision, "applied")
            self.assertEqual(stats.color_corrected_frames, stats.right_videos)
            self.assertEqual(stats.right_videos, FRAME_COUNT)
            self.assertIsNotNone(stats.color_report)
            self.assertGreater(stats.color_matches_after_filter, 0)

            reference_left = eye_of(frames[0], "left")
            source_right = eye_of(frames[0], "right")
            decoded_left = decode_topic(output, "/decxin/left_rgb/h264")[0]
            decoded_right = decode_topic(output, "/decxin/right_rgb/h264")[0]

            baseline = mean_abs(source_right, reference_left)
            after = mean_abs(decoded_left, reference_left)
            corrected = mean_abs(decoded_right, reference_left)
            # The left eye is only re-encoded; the right eye moves most of the
            # way to the reference eye's colour.
            self.assertLess(after, baseline * .3)
            self.assertLess(corrected, baseline * .5)

    def test_color_consistency_can_be_disabled(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames = [joined_frame(index) for index in range(FRAME_COUNT)]
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_joined_source(source, frames)

            stats = StereoSplitH264Converter(
                ConverterConfig(apply_color_consistency=False)).convert(source, output)

            self.assertEqual(stats.color_decision, "")
            self.assertEqual(stats.color_corrected_frames, 0)
            self.assertIsNone(stats.color_report)
            source_right = eye_of(frames[0], "right")
            decoded_right = decode_topic(output, "/decxin/right_rgb/h264")[0]
            self.assertLess(mean_abs(decoded_right, source_right), 8.0)

    def test_skips_correction_when_the_eyes_are_flat(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames = [joined_frame(0, textured=False) for _ in range(FRAME_COUNT)]
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_joined_source(source, frames)

            stats = self.convert(source, output)

            self.assertEqual(stats.color_decision, "insufficient")
            self.assertEqual(stats.color_corrected_frames, 0)
            self.assertEqual(stats.right_videos, FRAME_COUNT)
            source_right = eye_of(frames[0], "right")
            decoded_right = decode_topic(output, "/decxin/right_rgb/h264")[0]
            self.assertLess(mean_abs(decoded_right, source_right), 8.0)

    def test_spatial_field_path_runs_in_the_converter(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames = [joined_frame(index) for index in range(FRAME_COUNT)]
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_joined_source(source, frames)

            stats = self.convert(
                source, output, color_config=test_color_config(spatial_grid=3))

            self.assertEqual(stats.color_decision, "applied")
            self.assertEqual(stats.color_report["model"]["spatial_grid"], 3)
            self.assertEqual(stats.color_corrected_frames, stats.right_videos)

    def test_runner_publishes_the_colour_report_and_summary(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames = [joined_frame(index) for index in range(FRAME_COUNT)]
            source = root / "source.mcap"
            output_binding = root / "published"
            output_binding.mkdir()
            make_joined_source(source, frames)

            result = run_runner(source, output_binding, root / "scratch")

            self.assertEqual(result.returncode, 0, result.stderr)
            manifest = json.loads(
                (output_binding / "processing_manifest.json").read_text(encoding="utf-8"))
            metadata = json.loads(
                (output_binding / "metadata.yaml").read_text(encoding="utf-8"))
            summary = manifest["color_consistency"]
            # Four fixture frames are far below the production sampling
            # density, so the runner decides the eyes need no correction; the
            # applied decision itself is covered by the converter tests above.
            self.assertEqual(summary["decision"], "insufficient")
            self.assertEqual(summary["corrected_frames"], 0)
            self.assertEqual(
                summary["corrected_frames"], manifest["stats"]["color_corrected_frames"])
            self.assertNotIn("anchors", summary)
            self.assertNotIn("color_report", manifest["stats"])
            report = metadata["stats"]["color_report"]
            self.assertEqual(report["decision"], summary["decision"])
            self.assertEqual(metadata["color_consistency"], summary)


if __name__ == "__main__":
    unittest.main()
