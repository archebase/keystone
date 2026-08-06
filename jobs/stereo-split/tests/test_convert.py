# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

import json
from pathlib import Path
import re
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


def encode_h264_access_units(frames: list[np.ndarray]) -> list[bytes]:
    height, width = frames[0].shape[:2]
    encoded = subprocess.run(
        [
            "ffmpeg", "-hide_banner", "-loglevel", "error",
            "-f", "rawvideo", "-pixel_format", "bgr24",
            "-video_size", f"{width}x{height}", "-framerate", "60", "-i", "pipe:0",
            "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
            "-profile:v", "high", "-pix_fmt", "yuv420p", "-g", "1", "-bf", "0",
            "-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "pipe:1",
        ],
        input=b"".join(frame.tobytes() for frame in frames),
        capture_output=True,
        check=True,
    ).stdout
    starts = [match.start() for match in re.finditer(b"\\x00\\x00(?:\\x00)?\\x01\\x09", encoded)]
    access_units = [
        encoded[start : starts[index + 1] if index + 1 < len(starts) else len(encoded)]
        for index, start in enumerate(starts)
    ]
    if len(access_units) != len(frames):
        raise RuntimeError(
            f"H.264 fixture frame count mismatch: {len(access_units)} != {len(frames)}"
        )
    return access_units


def make_source(
    path: Path,
    frame_count: int = 3,
    *,
    source_format: str = "jpeg",
    source_formats: list[str] | None = None,
    source_frames: list[np.ndarray] | None = None,
    source_payloads: list[np.ndarray] | None = None,
    include_existing_imu: bool = False,
    log_times: list[int] | None = None,
    publish_times: list[int] | None = None,
    sequences: list[int] | None = None,
    header_stamps: list[tuple[int, int]] | None = None,
) -> None:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    messages = typestore.types
    frames = source_frames if source_frames is not None else [
        contract_frame(index) for index in range(frame_count)
    ]
    frame_count = len(source_payloads) if source_payloads is not None else len(frames)
    formats = source_formats or [source_format] * frame_count
    if len(formats) != frame_count:
        raise ValueError("source format count must match frame count")
    for name, values in (
        ("log time", log_times),
        ("publish time", publish_times),
        ("sequence", sequences),
        ("header stamp", header_stamps),
    ):
        if values is not None and len(values) != frame_count:
            raise ValueError(f"source {name} count must match frame count")
    h264_access_units = (
        encode_h264_access_units(frames)
        if source_payloads is None and "h264" in formats
        else []
    )
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
        existing_imu_channel = None
        if include_existing_imu:
            existing_imu_channel = writer.register_channel("/decxin/imu", "cdr", string_schema)
        for index in range(frame_count):
            message_format = formats[index]
            if source_payloads is not None:
                payload = source_payloads[index]
            elif message_format == "h264":
                payload = np.frombuffer(h264_access_units[index], dtype=np.uint8)
            else:
                ok, payload = cv2.imencode(
                    ".jpg", frames[index], [int(cv2.IMWRITE_JPEG_QUALITY), 100]
                )
                if not ok:
                    raise RuntimeError("failed to create JPEG fixture")
            timestamp = (
                log_times[index] if log_times is not None else (index + 1) * 1_000_000_000
            )
            publish_time = publish_times[index] if publish_times is not None else timestamp
            sequence = sequences[index] if sequences is not None else index
            stamp_seconds, stamp_nanos = (
                header_stamps[index] if header_stamps is not None else (index + 1, 0)
            )
            stamp = messages["builtin_interfaces/msg/Time"](
                sec=stamp_seconds, nanosec=stamp_nanos
            )
            header = messages["std_msgs/msg/Header"](stamp=stamp, frame_id="joined_camera")
            compressed = messages["sensor_msgs/msg/CompressedImage"](
                header=header, format=message_format, data=payload.reshape(-1)
            )
            writer.add_message(
                compressed_channel,
                timestamp,
                bytes(typestore.serialize_cdr(compressed, "sensor_msgs/msg/CompressedImage")),
                publish_time,
                sequence,
            )
            kept = messages["std_msgs/msg/String"](data=f"kept-{index}")
            writer.add_message(
                string_channel,
                timestamp + 1,
                bytes(typestore.serialize_cdr(kept, "std_msgs/msg/String")),
                timestamp + 1,
                index,
            )
            if existing_imu_channel is not None:
                writer.add_message(
                    existing_imu_channel,
                    timestamp + 2,
                    bytes(typestore.serialize_cdr(kept, "std_msgs/msg/String")),
                    timestamp + 2,
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
            with output.open("rb") as stream:
                physical_times = [
                    message.log_time
                    for _, _, message in make_reader(stream).iter_messages(log_time_order=False)
                ]
            self.assertEqual(physical_times, sorted(physical_times))

    def test_accepts_standard_ros_jpeg_format_description(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source, frame_count=2, source_format="bgr8; jpeg compressed bgr8")

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.decoded_images, 2)
            self.assertEqual(stats.left_videos, 2)
            self.assertEqual(stats.right_videos, 2)

    def test_converts_joined_h264_input(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            log_times = [1_000_000_101, 2_000_000_202, 3_000_000_303]
            publish_times = [7_000_000_707, 8_000_000_808, 9_000_000_909]
            sequences = [41, 73, 109]
            header_stamps = [(11, 111), (22, 222), (33, 333)]
            make_source(
                source,
                source_format="h264",
                log_times=log_times,
                publish_times=publish_times,
                sequences=sequences,
                header_stamps=header_stamps,
            )

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.input_messages, 3)
            self.assertEqual(stats.decoded_images, 3)
            self.assertEqual(stats.left_videos, 3)
            self.assertEqual(stats.right_videos, 3)
            self.assertEqual(stats.imu_messages, 22)
            with output.open("rb") as stream:
                by_topic: dict[str, list[object]] = {}
                for _, channel, message in make_reader(stream).iter_messages():
                    by_topic.setdefault(channel.topic, []).append(message)
            for topic in ("/decxin/left_rgb/h264", "/decxin/right_rgb/h264"):
                records = by_topic[topic]
                self.assertEqual([record.log_time for record in records], log_times)
                self.assertEqual([record.publish_time for record in records], publish_times)
                self.assertEqual([record.sequence for record in records], sequences)
                videos = [CompressedVideo.FromString(record.data) for record in records]
                self.assertEqual(
                    [(video.timestamp.seconds, video.timestamp.nanos) for video in videos],
                    header_stamps,
                )
            with output.open("rb") as stream:
                physical_times = [
                    message.log_time
                    for _, _, message in make_reader(stream).iter_messages(log_time_order=False)
                ]
            self.assertEqual(physical_times, sorted(physical_times))

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

    def test_replaces_existing_imu_topic_with_decoded_samples(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source, include_existing_imu=True)

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.imu_messages, 22)
            self.assertEqual(stats.copied_messages, 3)
            self.assertEqual(stats.copied_topics, 1)
            with output.open("rb") as stream:
                imu_records = [
                    (schema, channel, message)
                    for schema, channel, message in make_reader(stream).iter_messages()
                    if channel.topic == "/decxin/imu"
                ]
            self.assertEqual(len(imu_records), 22)
            self.assertTrue(
                all(schema.name == "sensor_msgs/msg/Imu" for schema, _, _ in imu_records)
            )

    def test_h264_input_replaces_existing_imu_topic_with_decoded_samples(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source, source_format="h264", include_existing_imu=True)

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.imu_messages, 22)
            self.assertEqual(stats.copied_messages, 3)
            self.assertEqual(stats.copied_topics, 1)
            with output.open("rb") as stream:
                imu_records = [
                    (schema, channel, message)
                    for schema, channel, message in make_reader(stream).iter_messages()
                    if channel.topic == "/decxin/imu"
                ]
            self.assertEqual(len(imu_records), 22)
            self.assertTrue(
                all(schema.name == "sensor_msgs/msg/Imu" for schema, _, _ in imu_records)
            )

    def test_rejects_jpeg_then_h264_input(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            make_source(source, frame_count=2, source_formats=["jpeg", "h264"])

            with self.assertRaisesRegex(RuntimeError, "cannot mix JPEG and H.264"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_h264_then_jpeg_input(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            make_source(source, frame_count=2, source_formats=["h264", "jpeg"])

            with self.assertRaisesRegex(RuntimeError, "cannot mix JPEG and H.264"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_unsupported_compressed_image_format(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            make_source(source, frame_count=1, source_format="png")

            with self.assertRaisesRegex(RuntimeError, "unsupported CompressedImage format"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_h264_input_smaller_than_required_geometry(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            undersized = np.zeros((1198, 4000, 3), dtype=np.uint8)
            make_source(source, source_format="h264", source_frames=[undersized])

            with self.assertRaisesRegex(RuntimeError, "required 4000x1200"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_multiple_h264_frames_in_one_input_message(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            access_units = encode_h264_access_units([contract_frame(0), contract_frame(1)])
            payload = np.frombuffer(b"".join(access_units), dtype=np.uint8)
            make_source(source, source_format="h264", source_payloads=[payload])

            with self.assertRaisesRegex(RuntimeError, "one decoded frame per input message"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_one_h264_frame_split_across_input_messages(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            access_unit = encode_h264_access_units([contract_frame(0)])[0]
            split_at = len(access_unit) // 2
            payloads = [
                np.frombuffer(access_unit[:split_at], dtype=np.uint8),
                np.frombuffer(access_unit[split_at:], dtype=np.uint8),
            ]
            make_source(source, source_format="h264", source_payloads=payloads)

            with self.assertRaisesRegex(RuntimeError, "one decoded frame per input message"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_compensating_h264_frame_counts_between_messages(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            access_units = encode_h264_access_units([contract_frame(0), contract_frame(1)])
            payloads = [
                np.frombuffer(b"".join(access_units), dtype=np.uint8),
                np.array([], dtype=np.uint8),
            ]
            make_source(source, source_format="h264", source_payloads=payloads)

            with self.assertRaisesRegex(RuntimeError, "one decoded frame per input message"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")


if __name__ == "__main__":
    unittest.main()
