# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import cv2
from mcap.reader import make_reader
from mcap.writer import Writer
import numpy as np
from rosbags.typesys import Stores, get_typestore


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from convert_mcap_stereo_h264 import (  # noqa: E402
    CompressedVideo,
    ConverterConfig,
    FOXGLOVE_SCHEMA_NAME,
    StereoSplitH264Converter,
)
sys.path.remove(str(JOB_ROOT))


def encode_metadata_line(frame: np.ndarray, row: int, payload: bytes) -> None:
    frame[row, :, :] = 255
    frame[row, 4:8, :] = 0
    for bit_index in range(128):
        bit = (payload[bit_index // 8] >> (bit_index % 8)) & 1
        start = 8 + bit_index * 8
        frame[row, start : start + 8, :] = 100 if bit else 0


def contract_frame(index: int) -> np.ndarray:
    frame = np.zeros((1200, 4000, 3), dtype=np.uint8)
    frame[:, 160:2080, :] = (20 + index, 90, 180)
    frame[:, 2080:4000, :] = (180, 50 + index, 20)
    exposure = (123).to_bytes(4, "big") + (456).to_bytes(4, "big")
    groups = [exposure + exposure]
    for sample in range(11):
        groups.append(
            (sample + 1).to_bytes(4, "big")
            + (1000).to_bytes(2, "big", signed=True)
            + (-2000).to_bytes(2, "big", signed=True)
            + (3000).to_bytes(2, "big", signed=True)
            + (4000).to_bytes(2, "big", signed=True)
            + (-5000).to_bytes(2, "big", signed=True)
            + (6000).to_bytes(2, "big", signed=True)
        )
    for group_index, group in enumerate(groups):
        encode_metadata_line(frame, 2 + group_index * 8, group)
    return frame


def make_source(path: Path, frame_count: int = 3) -> None:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    messages = typestore.types
    compressed_definition, _ = typestore.generate_msgdef(
        "sensor_msgs/msg/CompressedImage", ros_version=2
    )
    string_definition, _ = typestore.generate_msgdef("std_msgs/msg/String", ros_version=2)
    with path.open("wb") as stream:
        writer = Writer(stream)
        writer.start()
        compressed_schema = writer.register_schema(
            "sensor_msgs/msg/CompressedImage", "ros2msg", compressed_definition.encode()
        )
        compressed_channel = writer.register_channel(
            "/decxin/rgb/compressed", "cdr", compressed_schema
        )
        string_schema = writer.register_schema(
            "std_msgs/msg/String", "ros2msg", string_definition.encode()
        )
        string_channel = writer.register_channel("/kept", "cdr", string_schema)
        for index in range(frame_count):
            ok, jpeg = cv2.imencode(
                ".jpg", contract_frame(index), [int(cv2.IMWRITE_JPEG_QUALITY), 100]
            )
            if not ok:
                raise RuntimeError("failed to create JPEG fixture")
            timestamp = (index + 1) * 1_000_000_000
            stamp = messages["builtin_interfaces/msg/Time"](sec=index + 1, nanosec=0)
            header = messages["std_msgs/msg/Header"](stamp=stamp, frame_id="joined_camera")
            compressed = messages["sensor_msgs/msg/CompressedImage"](
                header=header, format="jpeg", data=jpeg.reshape(-1)
            )
            writer.add_message(
                compressed_channel,
                timestamp,
                bytes(typestore.serialize_cdr(compressed, "sensor_msgs/msg/CompressedImage")),
                timestamp,
                index,
            )
            kept = messages["std_msgs/msg/String"](data=f"kept-{index}")
            writer.add_message(
                string_channel,
                timestamp + 1,
                bytes(typestore.serialize_cdr(kept, "std_msgs/msg/String")),
                timestamp + 1,
                index,
            )
        writer.finish()


class ConvertTest(unittest.TestCase):
    def test_converts_joined_jpeg_and_preserves_other_topics(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source)

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.input_messages, 3)
            self.assertEqual(stats.decoded_images, 3)
            self.assertEqual(stats.left_videos, 3)
            self.assertEqual(stats.right_videos, 3)
            self.assertEqual(stats.imu_messages, 22)
            self.assertEqual(stats.copied_messages, 3)
            self.assertEqual(stats.copied_topics, 1)

            by_topic: dict[str, list[tuple[object, object, object]]] = {}
            with output.open("rb") as stream:
                reader = make_reader(stream)
                for schema, channel, message in reader.iter_messages():
                    by_topic.setdefault(channel.topic, []).append((schema, channel, message))
            self.assertNotIn("/decxin/rgb/compressed", by_topic)
            self.assertEqual(
                set(by_topic),
                {"/decxin/left_rgb/h264", "/decxin/right_rgb/h264", "/decxin/imu", "/kept"},
            )
            self.assertEqual(len(by_topic["/kept"]), 3)

            for topic, frame_id in (
                ("/decxin/left_rgb/h264", "decxin_left_camera"),
                ("/decxin/right_rgb/h264", "decxin_right_camera"),
            ):
                records = by_topic[topic]
                self.assertEqual([record[2].log_time for record in records], [10**9, 2 * 10**9, 3 * 10**9])
                stream_bytes = bytearray()
                for schema, channel, message in records:
                    self.assertEqual(schema.name, FOXGLOVE_SCHEMA_NAME)
                    self.assertEqual(schema.encoding, "protobuf")
                    self.assertEqual(channel.message_encoding, "protobuf")
                    video = CompressedVideo.FromString(message.data)
                    self.assertEqual(video.frame_id, frame_id)
                    self.assertEqual(video.format, "h264")
                    self.assertEqual(video.timestamp.seconds, message.log_time // 10**9)
                    self.assertRegex(video.data, b"^\\x00\\x00\\x00?\\x01\\x09")
                    stream_bytes.extend(video.data)
                h264 = root / f"{topic.rsplit('/', 2)[-2]}.h264"
                h264.write_bytes(stream_bytes)
                probe = subprocess.run(
                    [
                        "ffprobe", "-v", "error", "-select_streams", "v:0",
                        "-show_entries", "stream=codec_name,profile,width,height",
                        "-of", "json", str(h264),
                    ],
                    capture_output=True, text=True, check=True,
                )
                stream = json.loads(probe.stdout)["streams"][0]
                self.assertEqual(stream["codec_name"], "h264")
                self.assertEqual(stream["profile"], "High")
                self.assertEqual((stream["width"], stream["height"]), (1920, 1200))
                trace = subprocess.run(
                    [
                        "ffmpeg", "-hide_banner", "-loglevel", "debug", "-i", str(h264),
                        "-c", "copy", "-bsf:v", "trace_headers", "-f", "null", "-",
                    ],
                    capture_output=True, text=True, check=True,
                ).stderr
                # H.264 VUI frame rate is time_scale / (2 * num_units_in_tick).
                self.assertRegex(trace, r"num_units_in_tick\s+.*= 1")
                self.assertRegex(trace, r"time_scale\s+.*= 120")
                self.assertRegex(trace, r"fixed_frame_rate_flag\s+.*= 1")

    def test_rejects_existing_output_topic(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            with source.open("wb") as stream:
                writer = Writer(stream)
                writer.start()
                schema = writer.register_schema("bytes", "", b"")
                channel = writer.register_channel(
                    ConverterConfig().left_topic, "", schema
                )
                writer.add_message(channel, 1, b"collision", 1)
                writer.finish()
            with self.assertRaisesRegex(RuntimeError, "already contains output topic"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")


if __name__ == "__main__":
    unittest.main()
