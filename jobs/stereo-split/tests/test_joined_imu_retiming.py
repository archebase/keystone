# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""End-to-end checks for IMU re-timing on a joined H.264 capture."""

from __future__ import annotations

import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from mcap.reader import make_reader
from mcap.writer import IndexType, Writer
import numpy as np
from rosbags.typesys import Stores, get_typestore


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from convert_mcap_stereo_h264 import (  # noqa: E402
    ConverterConfig,
    StereoSplitH264Converter,
)
from imu_retiming import ImuRetimingConfig  # noqa: E402
sys.path.remove(str(JOB_ROOT))


FRAME_WIDTH = 4000
FRAME_HEIGHT = 1200
EYE_WIDTH = 1920
METADATA_WIDTH = 160
FRAME_COUNT = 6
PACKET_SIZE = 11
MESSAGES_PER_FRAME = 22
SPACING_US = 1664
FRAME_PERIOD_US = SPACING_US * (PACKET_SIZE - 1) * 2
ANCHOR_OFFSET_US = -16717
BASE_TIMESTAMP_US = 3_000_000_000


def encode_metadata_line(frame: np.ndarray, row: int, payload: bytes) -> None:
    """Write one 16-byte barcode group into one image row."""
    if len(payload) != 16:
        raise ValueError("metadata groups must contain 16 bytes")
    frame[row, :, :] = 255
    frame[row, 4:8, :] = 0
    for bit_index in range(128):
        bit = (payload[bit_index // 8] >> (bit_index % 8)) & 1
        start = 8 + bit_index * 8
        frame[row, start:start + 8, :] = 100 if bit else 0


def icm42688_group(timestamp_us: int, accel_mg: tuple[float, float, float]) -> bytes:
    def raw(value: float, scale: float) -> int:
        return int(round(value / scale))

    values = [raw(accel_mg[0], 4000.0 / 32768.0), raw(accel_mg[1], 4000.0 / 32768.0),
              raw(accel_mg[2], 4000.0 / 32768.0), 100, -200, 300]
    payload = timestamp_us.to_bytes(4, "big")
    for value in values:
        payload += int(value).to_bytes(2, "big", signed=True)
    return payload


def barcode_frame(sample_values: list[tuple[float, float, float]],
                  first_timestamp_us: int) -> np.ndarray:
    """Build one joined frame whose metadata column carries packet A."""
    frame = np.zeros((FRAME_HEIGHT, FRAME_WIDTH, 3), dtype=np.uint8)
    frame[:, :METADATA_WIDTH] = 255
    frame[:, METADATA_WIDTH:] = 90
    exposure_end = first_timestamp_us - ANCHOR_OFFSET_US
    header = (exposure_end - 7495).to_bytes(4, "big") + exposure_end.to_bytes(4, "big")
    encode_metadata_line(frame, 2, header + header)
    for index, values in enumerate(sample_values):
        encode_metadata_line(
            frame,
            2 + (index + 1) * 8,
            icm42688_group(first_timestamp_us + SPACING_US * index, values),
        )
    return frame


def sample_values(index: int) -> tuple[float, float, float]:
    return (150.0 + index * 0.4, -60.0 + index * 0.2, -950.0 + index * 0.1)


def build_fixture() -> tuple[list[np.ndarray], list[tuple[float, float, float]]]:
    """Return the joined frames and the matching source IMU message values."""
    frames = []
    messages: list[tuple[float, float, float]] = []
    for frame_index in range(FRAME_COUNT):
        first_sample = 20 * frame_index
        first_timestamp_us = BASE_TIMESTAMP_US + SPACING_US * (first_sample + 1)
        packet_a = [sample_values(first_sample - 1 + position) for position in range(PACKET_SIZE)]
        frames.append(barcode_frame(packet_a, first_timestamp_us))
        for position in range(MESSAGES_PER_FRAME):
            index = first_sample - 1 + position - (1 if position >= PACKET_SIZE else 0)
            messages.append(sample_values(index))
    return frames, messages


def encode_access_units(frames: list[np.ndarray]) -> list[bytes]:
    encoded = subprocess.run(
        [
            "ffmpeg", "-hide_banner", "-loglevel", "error",
            "-f", "rawvideo", "-pixel_format", "bgr24",
            "-video_size", f"{FRAME_WIDTH}x{FRAME_HEIGHT}", "-framerate", "30", "-i", "pipe:0",
            "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
            "-profile:v", "high", "-pix_fmt", "yuv420p", "-g", "1", "-bf", "0",
            "-crf", "1", "-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "pipe:1",
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


def write_source(path: Path, frames: list[np.ndarray],
                 messages: list[tuple[float, float, float]]) -> None:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    types = typestore.types
    units = encode_access_units(frames)
    image_definition, _ = typestore.generate_msgdef(
        "sensor_msgs/msg/CompressedImage", ros_version=2)
    imu_definition, _ = typestore.generate_msgdef("sensor_msgs/msg/Imu", ros_version=2)
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
            "sensor_msgs/msg/CompressedImage", "ros2msg", image_definition.encode())
        image_channel = writer.register_channel("/decxin/rgb/compressed", "cdr", image_schema)
        imu_schema = writer.register_schema(
            "sensor_msgs/msg/Imu", "ros2msg", imu_definition.encode())
        imu_channel = writer.register_channel("/decxin/imu", "cdr", imu_schema)

        for index, unit in enumerate(units):
            timestamp = 1_790_000_000_000_000_000 + index * FRAME_PERIOD_US * 1000
            stamp = types["builtin_interfaces/msg/Time"](sec=index, nanosec=0)
            image = types["sensor_msgs/msg/CompressedImage"](
                header=types["std_msgs/msg/Header"](stamp=stamp, frame_id="joined_camera"),
                format="h264",
                data=np.frombuffer(unit, dtype=np.uint8),
            )
            writer.add_message(
                image_channel, timestamp,
                bytes(typestore.serialize_cdr(image, "sensor_msgs/msg/CompressedImage")),
                timestamp, index,
            )
            base = index * MESSAGES_PER_FRAME
            for position in range(MESSAGES_PER_FRAME):
                ax, ay, az = messages[base + position]
                imu = types["sensor_msgs/msg/Imu"](
                    header=types["std_msgs/msg/Header"](stamp=stamp, frame_id="decxin_imu"),
                    orientation=types["geometry_msgs/msg/Quaternion"](x=0.0, y=0.0, z=0.0, w=1.0),
                    orientation_covariance=np.zeros(9, dtype=np.float64),
                    angular_velocity=types["geometry_msgs/msg/Vector3"](x=0.0, y=0.0, z=0.0),
                    angular_velocity_covariance=np.zeros(9, dtype=np.float64),
                    linear_acceleration=types["geometry_msgs/msg/Vector3"](
                        x=ax * 9.80665 / 1000.0,
                        y=ay * 9.80665 / 1000.0,
                        z=az * 9.80665 / 1000.0,
                    ),
                    linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
                )
                writer.add_message(
                    imu_channel, timestamp + position + 1,
                    bytes(typestore.serialize_cdr(imu, "sensor_msgs/msg/Imu")),
                    timestamp + position + 1, position,
                )
        writer.finish()


def read_imu(path: Path) -> list[tuple[int, int, tuple[float, float, float]]]:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    records = []
    with path.open("rb") as stream:
        for _, _, message in make_reader(stream).iter_messages(topics=["/decxin/imu"]):
            imu = typestore.deserialize_cdr(message.data, "sensor_msgs/msg/Imu")
            records.append((
                message.log_time,
                imu.header.stamp.sec * 1_000_000_000 + imu.header.stamp.nanosec,
                (imu.linear_acceleration.x, imu.linear_acceleration.y, imu.linear_acceleration.z),
            ))
    return records


def test_imu_config() -> ImuRetimingConfig:
    return ImuRetimingConfig(min_sampled_frames=4)


class JoinedImuRetimingTest(unittest.TestCase):
    def convert(self, source: Path, output: Path, **kwargs):
        return StereoSplitH264Converter(
            ConverterConfig(apply_color_consistency=False),
            imu_config=kwargs.pop("imu_config", test_imu_config()),
        ).convert(source, output)

    def test_retimes_and_drops_repeats_on_the_grid(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames, messages = build_fixture()
            source = root / "source.mcap"
            output = root / "output.mcap"
            write_source(source, frames, messages)

            stats = self.convert(source, output)

            self.assertEqual(stats.imu_retiming_decision, "applied")
            self.assertEqual(stats.imu_retimed_messages, FRAME_COUNT * 20 + 1)
            self.assertEqual(stats.imu_dropped_duplicates, FRAME_COUNT * 2 - 1)
            self.assertEqual(stats.imu_messages, stats.imu_retimed_messages)
            self.assertEqual(stats.imu_report["grid_spacing_us"], float(SPACING_US))
            self.assertEqual(stats.imu_report["frame_period_us"], float(FRAME_PERIOD_US))

            records = read_imu(output)
            self.assertEqual(len(records), stats.imu_retimed_messages)
            for log_time, stamp, _ in records:
                self.assertEqual(log_time, stamp)
            steps = [later[0] - earlier[0] for earlier, later in zip(records, records[1:])]
            self.assertTrue(all(step == SPACING_US * 1000 for step in steps))
            kept = [record[2] for record in records]
            cursor = 0
            for value in kept:
                while cursor < len(messages) and not all(
                    abs(actual - expected * 9.80665 / 1000.0) < 1e-6
                    for actual, expected in zip(value, messages[cursor])
                ):
                    cursor += 1
                self.assertLess(cursor, len(messages), "output value is not a source sample")
                cursor += 1

    def test_leaves_the_source_stream_untouched_when_the_grid_is_broken(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames, messages = build_fixture()
            for frame in frames:
                for row in range(2, 2 + 12 * 8, 8):
                    frame[row, :, :] = 90  # erase the barcode rows entirely
            source = root / "source.mcap"
            output = root / "output.mcap"
            write_source(source, frames, messages)

            stats = self.convert(source, output)

            self.assertNotEqual(stats.imu_retiming_decision, "applied")
            self.assertEqual(stats.imu_retiming_decision, "skipped")
            self.assertTrue(stats.imu_report["reason"])
            self.assertEqual(stats.imu_messages, len(messages))

    def test_can_be_disabled(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frames, messages = build_fixture()
            source = root / "source.mcap"
            output = root / "output.mcap"
            write_source(source, frames, messages)

            stats = StereoSplitH264Converter(
                ConverterConfig(apply_color_consistency=False, apply_imu_retiming=False)
            ).convert(source, output)

            self.assertEqual(stats.imu_retiming_decision, "")
            self.assertEqual(stats.imu_messages, len(messages))
            self.assertIsNone(stats.imu_report)


if __name__ == "__main__":
    unittest.main()
