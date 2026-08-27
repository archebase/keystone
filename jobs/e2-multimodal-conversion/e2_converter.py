#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Convert an Ego Portal E2 capture directory into H.264 MCAP outputs."""

from __future__ import annotations

import csv
from dataclasses import dataclass
from decimal import Decimal
import json
from pathlib import Path
import re
import subprocess
from typing import Iterator

import numpy as np

from google.protobuf import descriptor_pb2, descriptor_pool, message_factory, timestamp_pb2
from mcap.reader import make_reader
from mcap.writer import Writer
from rosbags.typesys import Stores, get_typestore


LEFT_TOPIC = "/camera/left/image/h264"
RIGHT_TOPIC = "/camera/right/image/h264"
IMU_TOPIC = "/imu/data"
FOXGLOVE_SCHEMA = "foxglove.CompressedVideo"


def foxglove_compressed_video() -> tuple[bytes, type]:
    file_descriptor = descriptor_pb2.FileDescriptorProto(
        name="foxglove/CompressedVideo.proto", package="foxglove", syntax="proto3",
        dependency=["google/protobuf/timestamp.proto"],
    )
    message = file_descriptor.message_type.add(name="CompressedVideo")
    fields = (
        ("timestamp", 1, descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE, ".google.protobuf.Timestamp"),
        ("frame_id", 2, descriptor_pb2.FieldDescriptorProto.TYPE_STRING, ""),
        ("data", 3, descriptor_pb2.FieldDescriptorProto.TYPE_BYTES, ""),
        ("format", 4, descriptor_pb2.FieldDescriptorProto.TYPE_STRING, ""),
    )
    for name, number, field_type, type_name in fields:
        field = message.field.add(name=name, number=number, type=field_type,
                                  label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL)
        if type_name:
            field.type_name = type_name
    pool = descriptor_pool.DescriptorPool()
    pool.AddSerializedFile(timestamp_pb2.DESCRIPTOR.serialized_pb)
    pool.Add(file_descriptor)
    cls = message_factory.GetMessageClass(pool.FindMessageTypeByName(FOXGLOVE_SCHEMA))
    descriptor_set = descriptor_pb2.FileDescriptorSet()
    descriptor_set.file.add().ParseFromString(timestamp_pb2.DESCRIPTOR.serialized_pb)
    descriptor_set.file.add().CopyFrom(file_descriptor)
    return descriptor_set.SerializeToString(), cls


FOXGLOVE_DESCRIPTOR, CompressedVideo = foxglove_compressed_video()


@dataclass(frozen=True)
class VideoFrame:
    timestamp_ns: int
    data: bytes
    sequence: int


@dataclass
class ConversionStats:
    left_video_frames: int = 0
    right_video_frames: int = 0
    imu_messages: int = 0
    dropped_left_video_frames: int = 0
    dropped_right_video_frames: int = 0


def _access_units(stream: Iterator[bytes]) -> Iterator[bytes]:
    """Split an Annex-B stream at AUD NAL units without buffering the stream."""
    buffer = bytearray()
    marker = re.compile(b"(?:\\x00\\x00\\x00\\x01|\\x00\\x00\\x01)\\x09")
    started = False
    for chunk in stream:
        buffer.extend(chunk)
        while True:
            matches = list(marker.finditer(buffer))
            if not matches:
                break
            if not started:
                del buffer[:matches[0].start()]
                started = True
                continue
            if matches[0].start() == 0:
                if len(matches) < 2:
                    break
                boundary = matches[1].start()
            else:
                boundary = matches[0].start()
            yield bytes(buffer[:boundary])
            del buffer[:boundary]
    if started and buffer:
        yield bytes(buffer)


def _ffmpeg_video(path: Path) -> Iterator[bytes]:
    command = [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", str(path),
        "-map", "0:v:0", "-c:v", "libx264", "-preset", "medium", "-profile:v", "high",
        "-pix_fmt", "yuv420p", "-bf", "0", "-g", "30", "-keyint_min", "30",
        "-sc_threshold", "0", "-b:v", "12M", "-maxrate", "12M", "-bufsize", "24M",
        "-x264-params", "aud=1:repeat-headers=1", "-an", "-f", "h264", "pipe:1",
    ]
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    assert process.stdout is not None
    try:
        yield from iter(lambda: process.stdout.read(256 * 1024), b"")
        if process.wait() != 0:
            error = process.stderr.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"ffmpeg failed for {path.name}: {error.strip()}")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        process.stdout.close()
        process.stderr.close()


def _video_timestamps(path: Path) -> list[int]:
    result = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_frames",
         "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", str(path)],
        check=True, capture_output=True, text=True,
    )
    frames = json.loads(result.stdout).get("frames", [])
    timestamps = []
    for frame in frames:
        value = frame.get("best_effort_timestamp_time")
        if value is not None:
            timestamps.append(int(Decimal(str(value)) * Decimal(1_000_000_000)))
    if not timestamps:
        raise RuntimeError(f"video has no timestamps: {path.name}")
    return timestamps


def _video_frames(path: Path) -> Iterator[VideoFrame]:
    timestamps = _video_timestamps(path)
    for sequence, data in enumerate(_access_units(_ffmpeg_video(path))):
        if sequence >= len(timestamps):
            raise RuntimeError(f"encoded frame count exceeds timestamp count: {path.name}")
        yield VideoFrame(timestamps[sequence], data, sequence)


def _csv_rows(path: Path) -> Iterator[tuple[int, tuple[float, float, float]]]:
    with path.open(newline="", encoding="utf-8") as stream:
        reader = csv.DictReader(stream)
        for row in reader:
            if not row.get("ts_ns") or any(not row.get(name) for name in reader.fieldnames[1:]):
                continue
            yield int(row["ts_ns"]), (float(row[reader.fieldnames[1]]),
                                       float(row[reader.fieldnames[2]]),
                                       float(row[reader.fieldnames[3]]))


def _imu_rows(root: Path) -> Iterator[tuple[int, tuple[float, ...]]]:
    accelerometer = _csv_rows(root / "Sensors/accel.csv")
    gyroscope = _csv_rows(root / "Sensors/gyro.csv")
    acceleration = next(accelerometer, None)
    rotation = next(gyroscope, None)
    while acceleration is not None and rotation is not None:
        if acceleration[0] == rotation[0]:
            yield acceleration[0], (*acceleration[1], *rotation[1])
            acceleration = next(accelerometer, None)
            rotation = next(gyroscope, None)
        elif acceleration[0] < rotation[0]:
            acceleration = next(accelerometer, None)
        else:
            rotation = next(gyroscope, None)


def _timestamp(ts_ns: int):
    seconds, nanos = divmod(ts_ns, 1_000_000_000)
    value = timestamp_pb2.Timestamp(seconds=seconds, nanos=nanos)
    return value


def _yaml_metadata(message_count: int, start_ns: int, end_ns: int,
                   topics: list[tuple[str, str, str, int]]) -> str:
    lines = [
        "rosbag2_bagfile_information:", "  version: 5", "  storage_identifier: mcap",
        f"  duration:\n    nanoseconds: {max(0, end_ns - start_ns)}",
        f"  starting_time:\n    nanoseconds_since_epoch: {start_ns}",
        f"  message_count: {message_count}", "  topics_with_message_count:",
    ]
    for topic, message_type, encoding, topic_count in topics:
        lines += ["  - topic_metadata:", f"      name: {topic}", f"      type: {message_type}",
                  f"      serialization_format: {encoding}", "      offered_qos_profiles: ''",
                  f"    message_count: {topic_count}"]
    lines += ["  compression_format: ''", "  compression_mode: ''", "  relative_file_paths:",
              "  - output_bag.mcap", "  files:", "  - path: output_bag.mcap", f"    starting_time:\n      nanoseconds_since_epoch: {start_ns}",
              f"    duration:\n      nanoseconds: {max(0, end_ns - start_ns)}", f"    message_count: {message_count}"]
    return "\n".join(lines) + "\n"


def convert(root: Path, output: Path, source_uri: str = "", source_size: int = 0,
            generation: int = 1, processor_image: str = "") -> dict[str, object]:
    root = root.resolve()
    output.mkdir(parents=True, exist_ok=True)
    left = root / "Camera0/video.mp4"
    right = root / "Camera1/video.mp4"
    required = [left, right, root / "Camera0/camera_params.json", root / "Camera1/camera_params.json",
                root / "Sensors/accel.csv", root / "Sensors/gyro.csv"]
    missing = [str(path.relative_to(root)) for path in required if not path.is_file()]
    if missing:
        raise RuntimeError(f"missing E2 input files: {', '.join(missing)}")

    typestore = get_typestore(Stores.ROS2_HUMBLE)
    imu_type = typestore.types["sensor_msgs/msg/Imu"]
    msg = typestore.types
    with (output / "output_bag.mcap").open("wb") as stream:
        writer = Writer(stream)
        writer.start(profile="", library="archebase e2 multimodal conversion")
        video_schema = writer.register_schema(FOXGLOVE_SCHEMA, "protobuf", FOXGLOVE_DESCRIPTOR)
        left_channel = writer.register_channel(LEFT_TOPIC, "protobuf", video_schema)
        right_channel = writer.register_channel(RIGHT_TOPIC, "protobuf", video_schema)
        imu_definition, _ = typestore.generate_msgdef("sensor_msgs/msg/Imu")
        imu_schema = writer.register_schema("sensor_msgs/msg/Imu", "ros2msg", imu_definition.encode())
        imu_channel = writer.register_channel(IMU_TOPIC, "cdr", imu_schema)
        stats = ConversionStats()
        left_frames = iter(_video_frames(left))
        right_frames = iter(_video_frames(right))

        left_frame = next(left_frames, None)
        right_frame = next(right_frames, None)
        imu = _imu_rows(root)
        imu_sample = next(imu, None)
        sequence = 0
        timestamps: list[int] = []

        def next_video_pair() -> tuple[int, VideoFrame, VideoFrame] | None:
            nonlocal left_frame, right_frame
            if left_frame is None or right_frame is None:
                return None
            pair = (max(left_frame.timestamp_ns, right_frame.timestamp_ns), left_frame, right_frame)
            left_frame = next(left_frames, None)
            right_frame = next(right_frames, None)
            return pair

        video_pair = next_video_pair()
        while video_pair is not None or imu_sample is not None:
            if video_pair is not None and (
                imu_sample is None or video_pair[0] <= imu_sample[0]
            ):
                ts, left_payload, right_payload = video_pair
                for channel, frame_id, data in (
                    (left_channel, "camera_left_optical", left_payload.data),
                    (right_channel, "camera_right_optical", right_payload.data),
                ):
                    video = CompressedVideo()
                    video.timestamp.seconds = ts // 1_000_000_000
                    video.timestamp.nanos = ts % 1_000_000_000
                    video.frame_id = frame_id
                    video.data = data
                    video.format = "h264"
                    writer.add_message(channel, ts, video.SerializeToString(), ts, sequence)
                stats.left_video_frames += 1
                stats.right_video_frames += 1
                video_pair = next_video_pair()
            else:
                if imu_sample is None:
                    raise AssertionError("IMU merge state is inconsistent")
                ts, values = imu_sample
                stamp = _timestamp(ts)
                imu_message = imu_type(
                    header=msg["std_msgs/msg/Header"](
                        stamp=msg["builtin_interfaces/msg/Time"](
                            sec=stamp.seconds, nanosec=stamp.nanos
                        ),
                        frame_id="imu",
                    ),
                    orientation=msg["geometry_msgs/msg/Quaternion"](
                        x=0.0, y=0.0, z=0.0, w=1.0
                    ),
                    orientation_covariance=np.array(
                        [-1.0] + [0.0] * 8, dtype=np.float64
                    ),
                    angular_velocity=msg["geometry_msgs/msg/Vector3"](
                        x=values[3], y=values[4], z=values[5]
                    ),
                    angular_velocity_covariance=np.zeros(9, dtype=np.float64),
                    linear_acceleration=msg["geometry_msgs/msg/Vector3"](
                        x=values[0], y=values[1], z=values[2]
                    ),
                    linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
                )
                writer.add_message(
                    imu_channel,
                    ts,
                    bytes(typestore.serialize_cdr(imu_message, "sensor_msgs/msg/Imu")),
                    ts,
                    sequence,
                )
                stats.imu_messages += 1
                imu_sample = next(imu, None)
            timestamps.append(ts)
            sequence += 1

        while left_frame is not None:
            stats.dropped_left_video_frames += 1
            left_frame = next(left_frames, None)
        while right_frame is not None:
            stats.dropped_right_video_frames += 1
            right_frame = next(right_frames, None)
        writer.finish()
    # The MCAP summary is the source of truth for the metadata counts.
    with (output / "output_bag.mcap").open("rb") as stream:
        summary = make_reader(stream).get_summary()
    message_counts = {
        channel.topic: summary.statistics.channel_message_counts.get(channel.id, 0)
        for channel in summary.channels.values()
    }
    total_message_count = sum(message_counts.values())
    calibration = {"camera0": json.loads((root / "Camera0/camera_params.json").read_text()),
                   "camera1": json.loads((root / "Camera1/camera_params.json").read_text())}
    (output / "calibration.json").write_text(json.dumps(calibration, indent=2, sort_keys=True) + "\n")
    start_ns, end_ns = (min(timestamps), max(timestamps)) if timestamps else (0, 0)
    topics = [(LEFT_TOPIC, FOXGLOVE_SCHEMA, "protobuf", message_counts[LEFT_TOPIC]),
              (RIGHT_TOPIC, FOXGLOVE_SCHEMA, "protobuf", message_counts[RIGHT_TOPIC]),
              (IMU_TOPIC, "sensor_msgs/msg/Imu", "cdr", message_counts[IMU_TOPIC])]
    (output / "metadata.yaml").write_text(_yaml_metadata(total_message_count, start_ns, end_ns, topics))
    return {"stats": stats.__dict__, "generation": generation, "processor_image": processor_image,
            "source": {"uri": source_uri, "size_bytes": source_size}, "output_format": "h264_ros2_mcap"}
