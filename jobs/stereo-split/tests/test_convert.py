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
from mcap.writer import IndexType, Writer
import numpy as np
from rosbags.typesys import Stores, get_typestore


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from convert_mcap_stereo_h264 import (  # noqa: E402
    CompressedVideo,
    ConverterConfig,
    FOXGLOVE_SCHEMA_NAME,
    StereoSplitH264Converter,
    _TimestampSample,
    _build_timestamp_repair_plan,
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


def orientation_frame() -> np.ndarray:
    frame = np.full((1200, 4000, 3), 80, dtype=np.uint8)
    frame[:240, 160:400, :] = 240
    frame[-240:, 1840:2080, :] = 10
    frame[:240, 3760:4000, :] = 240
    frame[-240:, 2080:2320, :] = 10
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
    include_empty_imu_channel: bool = False,
    include_empty_channel: bool = False,
    include_kept_topic: bool = True,
    include_summary: bool = True,
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
    imu_definition, _ = typestore.generate_msgdef("sensor_msgs/msg/Imu", ros_version=2)
    string_definition, _ = typestore.generate_msgdef("std_msgs/msg/String", ros_version=2)
    with path.open("wb") as stream:
        writer = Writer(
            stream,
            index_types=IndexType.ALL if include_summary else IndexType.NONE,
            repeat_channels=include_summary,
            repeat_schemas=include_summary,
            use_statistics=include_summary,
            use_summary_offsets=include_summary,
        )
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
        string_channel = (
            writer.register_channel("/kept", "cdr", string_schema)
            if include_kept_topic
            else None
        )
        existing_imu_channel = None
        if include_existing_imu or include_empty_imu_channel:
            imu_schema = writer.register_schema(
                "sensor_msgs/msg/Imu", "ros2msg", imu_definition.encode()
            )
            existing_imu_channel = writer.register_channel(
                "/decxin/imu", "cdr", imu_schema, metadata={"source": "original"}
            )
        if include_empty_channel:
            writer.register_channel(
                "/empty", "cdr", string_schema, metadata={"source": "original"}
            )
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
            if string_channel is not None:
                kept = messages["std_msgs/msg/String"](data=f"kept-{index}")
                writer.add_message(
                    string_channel,
                    timestamp + 1,
                    bytes(typestore.serialize_cdr(kept, "std_msgs/msg/String")),
                    timestamp + 1,
                    index,
                )
            if existing_imu_channel is not None and include_existing_imu:
                vector = messages["geometry_msgs/msg/Vector3"]
                quaternion = messages["geometry_msgs/msg/Quaternion"]
                imu = messages["sensor_msgs/msg/Imu"](
                    header=messages["std_msgs/msg/Header"](
                        stamp=stamp, frame_id="source_imu"
                    ),
                    orientation=quaternion(x=0.0, y=0.0, z=0.0, w=1.0),
                    orientation_covariance=np.full(9, index, dtype=np.float64),
                    angular_velocity=vector(x=float(index), y=2.0, z=3.0),
                    angular_velocity_covariance=np.zeros(9, dtype=np.float64),
                    linear_acceleration=vector(x=4.0, y=5.0, z=6.0),
                    linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
                )
                writer.add_message(
                    existing_imu_channel,
                    timestamp + 2,
                    bytes(typestore.serialize_cdr(imu, "sensor_msgs/msg/Imu")),
                    timestamp + 3,
                    100 + index,
                )
        writer.finish()


def regular_frame_times(frame_count: int = 30) -> list[int]:
    start_time = 1_800_000_000_000_000_000
    return [start_time + index * 16_666_667 for index in range(frame_count)]


def bursty_frame_times(frame_count: int = 30) -> list[int]:
    start_time = 1_800_000_000_000_000_000
    return [
        start_time + (index // 10) * 300_000_000 + (index % 10) * 100_000
        for index in range(frame_count)
    ]


def timestamp_samples(
    header_times: list[int],
    log_times: list[int],
    publish_times: list[int] | None = None,
) -> list[_TimestampSample]:
    return [
        _TimestampSample(log_time, publish_time, header_time)
        for header_time, log_time, publish_time in zip(
            header_times,
            log_times,
            publish_times or log_times,
        )
    ]


def make_timestamp_source(
    path: Path,
    header_times: list[int],
    log_times: list[int],
    *,
    publish_times: list[int] | None = None,
    include_kept_topic: bool = False,
) -> None:
    ok, payload = cv2.imencode(
        ".jpg",
        contract_frame(0),
        [int(cv2.IMWRITE_JPEG_QUALITY), 100],
    )
    if not ok:
        raise RuntimeError("failed to create JPEG timestamp fixture")
    make_source(
        path,
        source_payloads=[payload] * len(header_times),
        include_kept_topic=include_kept_topic,
        log_times=log_times,
        publish_times=publish_times or log_times,
        header_stamps=[divmod(timestamp, 1_000_000_000) for timestamp in header_times],
    )


def topic_records(path: Path, topic: str) -> list[tuple[object, ...]]:
    records: list[tuple[object, ...]] = []
    with path.open("rb") as stream:
        for schema, channel, message in make_reader(stream).iter_messages(topics=[topic]):
            records.append(
                (
                    schema.name if schema is not None else None,
                    schema.encoding if schema is not None else None,
                    bytes(schema.data) if schema is not None else b"",
                    channel.message_encoding,
                    dict(channel.metadata),
                    message.log_time,
                    message.publish_time,
                    message.sequence,
                    bytes(message.data),
                )
            )
    return records


def decode_h264_frame(data: bytes, width: int = 1920, height: int = 1200) -> np.ndarray:
    decoded = subprocess.run(
        [
            "ffmpeg", "-hide_banner", "-loglevel", "error",
            "-f", "h264", "-i", "pipe:0", "-frames:v", "1",
            "-pix_fmt", "bgr24", "-f", "rawvideo", "pipe:1",
        ],
        input=data,
        capture_output=True,
        check=True,
    ).stdout
    expected_bytes = width * height * 3
    if len(decoded) != expected_bytes:
        raise RuntimeError(
            f"decoded H.264 frame size mismatch: {len(decoded)} != {expected_bytes}"
        )
    return np.frombuffer(decoded, dtype=np.uint8).reshape(height, width, 3)


def output_video_frame(path: Path, topic: str) -> np.ndarray:
    record = topic_records(path, topic)[0]
    return decode_h264_frame(CompressedVideo.FromString(record[8]).data)


def summary_channel(path: Path, topic: str) -> tuple[object, ...] | None:
    with path.open("rb") as stream:
        summary = make_reader(stream).get_summary()
    if summary is None:
        return None
    for channel in summary.channels.values():
        if channel.topic != topic:
            continue
        schema = summary.schemas.get(channel.schema_id) if channel.schema_id else None
        return (
            schema.name if schema is not None else None,
            schema.encoding if schema is not None else None,
            bytes(schema.data) if schema is not None else b"",
            channel.message_encoding,
            dict(channel.metadata),
        )
    return None


class ConvertTest(unittest.TestCase):
    def test_timestamp_plan_repairs_publish_only(self) -> None:
        header_times = regular_frame_times()
        log_times = [timestamp + 5_000_000 for timestamp in header_times]
        publish_times = bursty_frame_times()

        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, log_times, publish_times)
        )

        self.assertTrue(plan.applied)
        self.assertFalse(plan.log_repair_applied)
        self.assertTrue(plan.publish_repair_applied)
        self.assertEqual(plan.reason, "publish_bursty")
        repaired = [plan.video_message_times(sample) for sample in plan.samples]
        self.assertEqual([times[0] for times in repaired], log_times)
        self.assertEqual(
            [times[1] for times in repaired],
            [publish_times[0] + timestamp - header_times[0] for timestamp in header_times],
        )

    def test_timestamp_plan_repairs_bursty_prefix_in_otherwise_healthy_file(self) -> None:
        frame_count = 1_000
        bursty_frames = 150
        header_times = regular_frame_times(frame_count)
        bursty_prefix = bursty_frame_times(bursty_frames)
        log_times = bursty_prefix + [
            bursty_prefix[-1] + header_time - header_times[bursty_frames - 1]
            for header_time in header_times[bursty_frames:]
        ]

        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, log_times)
        )

        self.assertTrue(plan.applied)
        self.assertEqual(plan.reason, "log_bursty,publish_bursty")

    def test_timestamp_plan_preserves_healthy_outer_times(self) -> None:
        header_times = regular_frame_times()
        log_times = [timestamp + 5_000_000 for timestamp in header_times]
        publish_times = [timestamp + 1_000_000 for timestamp in log_times]

        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, log_times, publish_times)
        )

        self.assertFalse(plan.applied)
        self.assertEqual(plan.reason, "outer_timestamps_healthy")

    def test_timestamp_plan_rejects_non_monotonic_header(self) -> None:
        header_times = regular_frame_times()
        header_times[15] = header_times[14]

        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, bursty_frame_times())
        )

        self.assertFalse(plan.applied)
        self.assertEqual(plan.reason, "header_non_monotonic")

    def test_timestamp_plan_repairs_non_monotonic_log_times(self) -> None:
        header_times = regular_frame_times()
        log_times = [timestamp + 5_000_000 for timestamp in header_times]
        log_times[15] = log_times[14]

        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, log_times)
        )

        self.assertTrue(plan.applied)
        self.assertTrue(plan.log_repair_applied)
        self.assertEqual(plan.reason, "log_non_monotonic,publish_non_monotonic")
        self.assertEqual(
            [plan.video_message_times(sample)[0] for sample in plan.samples],
            [log_times[0] + timestamp - header_times[0] for timestamp in header_times],
        )

    def test_timestamp_cursor_aligns_copied_messages_with_duplicate_outer_times(self) -> None:
        header_times = regular_frame_times()
        log_times = [timestamp + 5_000_000 for timestamp in header_times]
        log_times[15] = log_times[14]
        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, log_times)
        )

        cursor = plan.new_cursor()
        for sample in plan.samples:
            video_log, video_publish = cursor.video_message_times(sample)
            copied_log, copied_publish = cursor.message_times(
                sample.log_time + 1, sample.publish_time + 1
            )
            self.assertEqual(copied_log, video_log + 1)
            self.assertEqual(copied_publish, video_publish + 1)

    def test_timestamp_plan_repairs_outer_clock_rate_mismatch(self) -> None:
        header_times = regular_frame_times()
        log_times = [
            header_times[0] + (timestamp - header_times[0]) * 3
            for timestamp in header_times
        ]

        plan = _build_timestamp_repair_plan(
            timestamp_samples(header_times, log_times)
        )

        self.assertTrue(plan.applied)
        self.assertEqual(plan.reason, "log_rate_mismatch,publish_rate_mismatch")

    def test_repairs_bursty_container_and_copied_topic_times(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            frame_count = 30
            header_times = regular_frame_times(frame_count)
            log_times = bursty_frame_times(frame_count)
            make_timestamp_source(
                source,
                header_times,
                log_times=log_times,
                include_kept_topic=True,
            )

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertTrue(stats.timestamp_repair_applied)
            self.assertTrue(stats.timestamp_log_repair_applied)
            self.assertTrue(stats.timestamp_publish_repair_applied)
            self.assertEqual(stats.timestamp_repair_reason, "log_bursty,publish_bursty")
            self.assertEqual(stats.timestamp_repaired_messages, frame_count * 2)
            expected_times = [
                log_times[0] + timestamp - header_times[0] for timestamp in header_times
            ]
            video_times = [record[5] for record in topic_records(output, "/decxin/left_rgb/h264")]
            self.assertEqual(video_times, expected_times)
            kept_times = [record[5] for record in topic_records(output, "/kept")]
            for index in range(frame_count - 1):
                self.assertLessEqual(video_times[index], kept_times[index])
                self.assertLess(kept_times[index], video_times[index + 1])
            self.assertGreaterEqual(kept_times[-1], video_times[-1])
            with output.open("rb") as stream:
                physical_times = [
                    message.log_time
                    for _, _, message in make_reader(stream).iter_messages(log_time_order=False)
                ]
            self.assertEqual(physical_times, sorted(physical_times))

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
            self.assertEqual(topic_records(output, "/kept"), topic_records(source, "/kept"))

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
                include_existing_imu=True,
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
            self.assertEqual(stats.imu_messages, 3)
            self.assertEqual(stats.copied_messages, 6)
            self.assertEqual(stats.copied_topics, 2)
            self.assertEqual(
                topic_records(output, "/decxin/imu"),
                topic_records(source, "/decxin/imu"),
            )
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

    def test_h264_input_preserves_eye_orientation(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(
                source,
                source_format="h264",
                source_frames=[orientation_frame()],
                include_existing_imu=True,
            )

            StereoSplitH264Converter().convert(source, output)

            left = output_video_frame(output, "/decxin/left_rgb/h264")
            self.assertGreater(left[:200, :200].mean(), left[-200:, -200:].mean() + 100)

            right = output_video_frame(output, "/decxin/right_rgb/h264")
            self.assertGreater(right[:200, -200:].mean(), right[-200:, :200].mean() + 100)

    def test_jpeg_input_rotates_each_eye_180_degrees(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(
                source,
                source_format="jpeg",
                source_frames=[orientation_frame()],
                include_existing_imu=True,
            )

            StereoSplitH264Converter().convert(source, output)

            left = output_video_frame(output, "/decxin/left_rgb/h264")
            self.assertGreater(left[-200:, -200:].mean(), left[:200, :200].mean() + 100)

            right = output_video_frame(output, "/decxin/right_rgb/h264")
            self.assertGreater(right[-200:, :200].mean(), right[:200, -200:].mean() + 100)

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

    def test_preserves_existing_imu_topic_instead_of_decoding_jpeg_pixels(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source, include_existing_imu=True)

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.imu_messages, 3)
            self.assertEqual(stats.copied_messages, 6)
            self.assertEqual(stats.copied_topics, 2)
            self.assertEqual(
                topic_records(output, "/decxin/imu"),
                topic_records(source, "/decxin/imu"),
            )

    def test_h264_input_preserves_existing_imu_without_decoding_pixels(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source, source_format="h264", include_existing_imu=True)

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.imu_messages, 3)
            self.assertEqual(stats.copied_messages, 6)
            self.assertEqual(stats.copied_topics, 2)
            self.assertEqual(
                topic_records(output, "/decxin/imu"),
                topic_records(source, "/decxin/imu"),
            )

    def test_preserves_empty_source_channel(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(source, include_empty_channel=True)

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.copied_messages, 3)
            self.assertEqual(stats.copied_topics, 2)
            self.assertEqual(summary_channel(output, "/empty"), summary_channel(source, "/empty"))

    def test_rejects_h264_input_without_source_imu(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            make_source(source, frame_count=1, source_format="h264")

            with self.assertRaisesRegex(RuntimeError, "H.264 input requires source IMU topic"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_empty_source_imu_channel(self) -> None:
        for source_format in ("jpeg", "h264"):
            with self.subTest(source_format=source_format), tempfile.TemporaryDirectory() as temp_dir:
                root = Path(temp_dir)
                source = root / "source.mcap"
                make_source(
                    source,
                    frame_count=1,
                    source_format=source_format,
                    include_empty_imu_channel=True,
                )

                with self.assertRaisesRegex(
                    RuntimeError, "source IMU topic contains no messages"
                ):
                    StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_preserves_source_channels_without_mcap_summary(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            make_source(
                source,
                source_format="h264",
                include_existing_imu=True,
                include_summary=False,
            )
            with source.open("rb") as stream:
                self.assertIsNone(make_reader(stream).get_summary())

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.copied_messages, 6)
            self.assertEqual(stats.copied_topics, 2)
            self.assertEqual(stats.imu_messages, 3)
            self.assertEqual(topic_records(output, "/kept"), topic_records(source, "/kept"))
            self.assertEqual(
                topic_records(output, "/decxin/imu"),
                topic_records(source, "/decxin/imu"),
            )

    def test_skips_h264_frames_before_first_idr(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            output = root / "output.mcap"
            access_units = encode_h264_access_units(
                [contract_frame(0), contract_frame(1), contract_frame(2)]
            )
            non_idr = np.frombuffer(
                b"\x00\x00\x00\x01\x09\x30\x00\x00\x00\x01\x41\x80",
                dtype=np.uint8,
            )
            payloads = [non_idr, non_idr] + [
                np.frombuffer(access_unit, dtype=np.uint8) for access_unit in access_units
            ]
            make_source(
                source,
                source_format="h264",
                source_payloads=payloads,
                include_existing_imu=True,
            )

            stats = StereoSplitH264Converter().convert(source, output)

            self.assertEqual(stats.input_messages, 5)
            self.assertEqual(stats.decoded_images, 3)
            self.assertEqual(stats.skipped_messages, 2)
            self.assertEqual(stats.left_videos, 3)
            self.assertEqual(stats.right_videos, 3)
            self.assertEqual(stats.imu_messages, 5)
            with output.open("rb") as stream:
                left_times = [
                    message.log_time
                    for _, _, message in make_reader(stream).iter_messages(
                        topics=["/decxin/left_rgb/h264"]
                    )
                ]
            self.assertEqual(left_times, [3 * 10**9, 4 * 10**9, 5 * 10**9])

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
            make_source(
                source,
                frame_count=2,
                source_formats=["h264", "jpeg"],
                include_existing_imu=True,
            )

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
            make_source(
                source,
                source_format="h264",
                source_frames=[undersized],
                include_existing_imu=True,
            )

            with self.assertRaisesRegex(RuntimeError, "required 4000x1200"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")

    def test_rejects_multiple_h264_frames_in_one_input_message(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            access_units = encode_h264_access_units([contract_frame(0), contract_frame(1)])
            payload = np.frombuffer(b"".join(access_units), dtype=np.uint8)
            make_source(
                source,
                source_format="h264",
                source_payloads=[payload],
                include_existing_imu=True,
            )

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
            make_source(
                source,
                source_format="h264",
                source_payloads=payloads,
                include_existing_imu=True,
            )

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
            make_source(
                source,
                source_format="h264",
                source_payloads=payloads,
                include_existing_imu=True,
            )

            with self.assertRaisesRegex(RuntimeError, "one decoded frame per input message"):
                StereoSplitH264Converter().convert(source, root / "output.mcap")


if __name__ == "__main__":
    unittest.main()
