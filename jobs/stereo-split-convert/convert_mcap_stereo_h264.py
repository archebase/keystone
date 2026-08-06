#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Convert one DECXIN joined JPEG topic to stereo H.264 and IMU MCAP topics."""

from __future__ import annotations

from collections import deque
from dataclasses import asdict, dataclass
import heapq
import os
from pathlib import Path
import queue
import re
import subprocess
import threading
from typing import BinaryIO

import cv2
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory, timestamp_pb2
from mcap.reader import make_reader
from mcap.writer import CompressionType, Writer
import numpy as np
from rosbags.typesys import Stores, get_typestore

from imu_decoder import ImuSample, decode_imu_from_bgr


FOXGLOVE_SCHEMA_NAME = "foxglove.CompressedVideo"
OUTPUT_FORMAT = "h264"
AUD_PATTERN = re.compile(b"(?:\\x00\\x00\\x00\\x01|\\x00\\x00\\x01)\\x09")
NAL_PATTERN = re.compile(b"(?:\\x00\\x00\\x00\\x01|\\x00\\x00\\x01)(.)", re.DOTALL)


@dataclass(frozen=True)
class ConverterConfig:
    input_topic: str = "/decxin/rgb/compressed"
    left_topic: str = "/decxin/left_rgb/h264"
    right_topic: str = "/decxin/right_rgb/h264"
    imu_topic: str = "/decxin/imu"
    metadata_width: int = 160
    eye_width: int = 1920
    eye_height: int = 1200
    left_frame_id: str = "decxin_left_camera"
    right_frame_id: str = "decxin_right_camera"
    imu_frame_id: str = "decxin_imu"
    nominal_fps: int = 60
    target_bitrate: str = "12M"
    max_bitrate: str = "16M"
    buffer_size: str = "24M"
    gop: int = 20


@dataclass
class ConvertStats:
    input_messages: int = 0
    decoded_images: int = 0
    left_videos: int = 0
    right_videos: int = 0
    imu_messages: int = 0
    copied_messages: int = 0
    copied_topics: int = 0
    skipped_messages: int = 0


@dataclass(frozen=True)
class VideoMetadata:
    log_time: int
    publish_time: int
    sequence: int
    seconds: int
    nanos: int


class OrderedMessageWriter:
    """Buffer delayed encoder output and write MCAP messages in log-time order."""

    def __init__(self, writer: Writer) -> None:
        self.writer = writer
        self.pending: list[tuple[int, int, int, bytes, int, int]] = []
        self.order = 0

    def add_message(
        self,
        channel_id: int,
        log_time: int,
        data: bytes,
        publish_time: int,
        sequence: int = 0,
    ) -> None:
        heapq.heappush(
            self.pending,
            (log_time, self.order, channel_id, data, publish_time, sequence),
        )
        self.order += 1

    def flush_before(self, log_time: int) -> None:
        while self.pending and self.pending[0][0] < log_time:
            self._write_next()

    def flush_all(self) -> None:
        while self.pending:
            self._write_next()

    def _write_next(self) -> None:
        log_time, _, channel_id, data, publish_time, sequence = heapq.heappop(self.pending)
        self.writer.add_message(channel_id, log_time, data, publish_time, sequence)


def _foxglove_descriptor() -> tuple[bytes, type]:
    file_descriptor = descriptor_pb2.FileDescriptorProto()
    file_descriptor.name = "foxglove/CompressedVideo.proto"
    file_descriptor.package = "foxglove"
    file_descriptor.syntax = "proto3"
    file_descriptor.dependency.append("google/protobuf/timestamp.proto")
    message = file_descriptor.message_type.add()
    message.name = "CompressedVideo"
    for name, number, field_type, type_name in (
        ("timestamp", 1, descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE, ".google.protobuf.Timestamp"),
        ("frame_id", 2, descriptor_pb2.FieldDescriptorProto.TYPE_STRING, ""),
        ("data", 3, descriptor_pb2.FieldDescriptorProto.TYPE_BYTES, ""),
        ("format", 4, descriptor_pb2.FieldDescriptorProto.TYPE_STRING, ""),
    ):
        field = message.field.add()
        field.name = name
        field.number = number
        field.label = descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL
        field.type = field_type
        if type_name:
            field.type_name = type_name

    descriptor_set = descriptor_pb2.FileDescriptorSet()
    timestamp_file = descriptor_set.file.add()
    timestamp_file.ParseFromString(timestamp_pb2.DESCRIPTOR.serialized_pb)
    descriptor_set.file.add().CopyFrom(file_descriptor)

    pool = descriptor_pool.DescriptorPool()
    pool.AddSerializedFile(timestamp_pb2.DESCRIPTOR.serialized_pb)
    pool.Add(file_descriptor)
    cls = message_factory.GetMessageClass(pool.FindMessageTypeByName(FOXGLOVE_SCHEMA_NAME))
    return descriptor_set.SerializeToString(), cls


FOXGLOVE_DESCRIPTOR_SET, CompressedVideo = _foxglove_descriptor()


def h264_nal_types(access_unit: bytes) -> list[int]:
    return [match.group(1)[0] & 0x1F for match in NAL_PATTERN.finditer(access_unit)]


class AnnexBAccessUnitReader(threading.Thread):
    """Read an Annex-B stream and emit one access unit per AUD-delimited frame."""

    def __init__(self, stream: BinaryIO, output: queue.Queue[bytes | BaseException | None]) -> None:
        super().__init__(daemon=True)
        self.stream = stream
        self.output = output

    def run(self) -> None:
        buffer = bytearray()
        try:
            while chunk := self.stream.read(256 * 1024):
                buffer.extend(chunk)
                starts = [match.start() for match in AUD_PATTERN.finditer(buffer)]
                while len(starts) >= 2:
                    boundary = starts[1]
                    self.output.put(bytes(buffer[:boundary]))
                    del buffer[:boundary]
                    starts = [match.start() for match in AUD_PATTERN.finditer(buffer)]
            first = AUD_PATTERN.search(buffer)
            if first is not None and len(buffer) > first.start():
                self.output.put(bytes(buffer[first.start() :]))
        except BaseException as error:  # propagate reader failures to the converter thread
            self.output.put(error)
        finally:
            self.output.put(None)


class RawVideoFrameReader(threading.Thread):
    """Read fixed-size BGR frames from FFmpeg without blocking its stdout pipe."""

    def __init__(
        self,
        stream: BinaryIO,
        frame_bytes: int,
        output: queue.Queue[bytes | BaseException | None],
    ) -> None:
        super().__init__(daemon=True)
        self.stream = stream
        self.frame_bytes = frame_bytes
        self.output = output

    def run(self) -> None:
        try:
            while True:
                frame = bytearray(self.frame_bytes)
                view = memoryview(frame)
                offset = 0
                while offset < self.frame_bytes:
                    size = self.stream.readinto(view[offset:])
                    if not size:
                        if offset:
                            raise RuntimeError(
                                f"FFmpeg returned a truncated raw frame: "
                                f"{offset}/{self.frame_bytes} bytes"
                            )
                        return
                    offset += size
                self.output.put(bytes(frame))
        except BaseException as error:  # propagate reader failures to the converter thread
            self.output.put(error)
        finally:
            self.output.put(None)


class H264FrameDecoder:
    """Decode an Annex-B H.264 access-unit stream to fixed-size BGR frames."""

    def __init__(self, width: int, height: int) -> None:
        self.width = width
        self.height = height
        self.frame_bytes = width * height * 3
        self.submitted = 0
        self.received = 0
        self.closed = False
        self.output: queue.Queue[bytes | BaseException | None] = queue.Queue()
        self.process = subprocess.Popen(
            [
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
                "-f", "h264", "-i", "pipe:0",
                "-map", "0:v:0", "-fps_mode", "passthrough",
                "-pix_fmt", "bgr24", "-f", "rawvideo", "pipe:1",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        if self.process.stdin is None or self.process.stdout is None:
            raise RuntimeError("FFmpeg H.264 decoder pipes were not created")
        self.reader = RawVideoFrameReader(self.process.stdout, self.frame_bytes, self.output)
        self.reader.start()

    def submit(self, access_unit: bytes) -> list[np.ndarray]:
        if self.closed:
            raise RuntimeError("cannot submit an H.264 frame after decoder close")
        try:
            self.process.stdin.write(access_unit)
            self.process.stdin.flush()
        except BrokenPipeError as error:
            raise RuntimeError(self._ffmpeg_error("FFmpeg stopped while accepting H.264")) from error
        self.submitted += 1
        return self._drain_available()

    def _as_frame(self, data: bytes) -> np.ndarray:
        self.received += 1
        return np.frombuffer(data, dtype=np.uint8).reshape(self.height, self.width, 3)

    def _take(self) -> np.ndarray:
        item = self.output.get(timeout=120)
        if isinstance(item, BaseException):
            raise RuntimeError("failed to read FFmpeg decoded video") from item
        if item is None:
            raise RuntimeError("FFmpeg decoded fewer H.264 frames than expected")
        return self._as_frame(item)

    def _drain_available(self) -> list[np.ndarray]:
        frames: list[np.ndarray] = []
        while True:
            try:
                item = self.output.get_nowait()
            except queue.Empty:
                return frames
            if isinstance(item, BaseException):
                raise RuntimeError("failed to read FFmpeg decoded video") from item
            if item is None:
                if self.received != self.submitted:
                    raise RuntimeError("FFmpeg decoded fewer H.264 frames than expected")
                return frames
            frames.append(self._as_frame(item))

    def finish(self) -> list[np.ndarray]:
        if self.closed:
            return []
        self.closed = True
        try:
            self.process.stdin.close()
            remaining: list[np.ndarray] = []
            while self.received < self.submitted:
                remaining.append(self._take())
            return_code = self.process.wait(timeout=120)
            self.reader.join(timeout=5)
            if return_code != 0:
                raise RuntimeError(self._ffmpeg_error(f"FFmpeg decoder exited with status {return_code}"))
            return remaining
        except BaseException:
            self._terminate()
            raise
        finally:
            self._close_streams()

    def abort(self) -> None:
        self.closed = True
        self._terminate()
        self._close_streams()

    def _ffmpeg_error(self, prefix: str) -> str:
        details = b""
        if self.process.stderr is not None:
            details = self.process.stderr.read()
        text = details.decode("utf-8", errors="replace").strip()
        return f"{prefix}: {text}" if text else prefix

    def _terminate(self) -> None:
        try:
            if not self.process.stdin.closed:
                self.process.stdin.close()
        except (BrokenPipeError, OSError):
            pass
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait()

    def _close_streams(self) -> None:
        if self.process.stdout is not None and not self.process.stdout.closed:
            self.process.stdout.close()
        if self.process.stderr is not None and not self.process.stderr.closed:
            self.process.stderr.close()


class DualH264Encoder:
    """Encode both eye crops using one persistent FFmpeg process and one raw-frame copy."""

    def __init__(self, width: int, height: int, config: ConverterConfig) -> None:
        self.config = config
        self.width = width
        self.height = height
        self.submitted = 0
        self.received = 0
        self.closed = False
        self.left_queue: queue.Queue[bytes | BaseException | None] = queue.Queue()
        self.right_queue: queue.Queue[bytes | BaseException | None] = queue.Queue()
        self.left_pending: deque[bytes] = deque()
        self.right_pending: deque[bytes] = deque()

        left_read, left_write = os.pipe()
        right_read, right_write = os.pipe()
        self.left_stream = os.fdopen(left_read, "rb", buffering=0)
        self.right_stream = os.fdopen(right_read, "rb", buffering=0)
        filter_graph = (
            "[0:v]split=2[left_input][right_input];"
            f"[left_input]crop={config.eye_width}:{config.eye_height}:"
            f"{config.metadata_width}:0,hflip,vflip,format=yuv420p[left];"
            f"[right_input]crop={config.eye_width}:{config.eye_height}:"
            f"{config.metadata_width + config.eye_width}:0,"
            "hflip,vflip,format=yuv420p[right]"
        )
        common = [
            "-c:v", "libx264",
            "-preset", "veryfast",
            "-tune", "zerolatency",
            "-profile:v", "high",
            "-pix_fmt", "yuv420p",
            "-r", str(config.nominal_fps),
            "-b:v", config.target_bitrate,
            "-maxrate", config.max_bitrate,
            "-bufsize", config.buffer_size,
            "-g", str(config.gop),
            "-keyint_min", str(config.gop),
            "-sc_threshold", "0",
            "-bf", "0",
            "-x264-params", "aud=1:repeat-headers=1:nal-hrd=vbr",
            "-flush_packets", "1",
            "-f", "h264",
        ]
        command = [
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
            "-f", "rawvideo", "-pixel_format", "bgr24",
            "-video_size", f"{width}x{height}",
            "-framerate", str(config.nominal_fps), "-i", "pipe:0",
            "-filter_complex", filter_graph,
            "-map", "[left]", *common, f"pipe:{left_write}",
            "-map", "[right]", *common, f"pipe:{right_write}",
        ]
        self.command = command
        try:
            self.process = subprocess.Popen(
                command,
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                pass_fds=(left_write, right_write),
            )
        finally:
            os.close(left_write)
            os.close(right_write)
        if self.process.stdin is None:
            raise RuntimeError("FFmpeg stdin was not created")
        self.left_reader = AnnexBAccessUnitReader(self.left_stream, self.left_queue)
        self.right_reader = AnnexBAccessUnitReader(self.right_stream, self.right_queue)
        self.left_reader.start()
        self.right_reader.start()

    def submit(self, frame: np.ndarray) -> list[tuple[bytes, bytes]]:
        if self.closed:
            raise RuntimeError("cannot submit a frame after encoder close")
        if frame.shape != (self.height, self.width, 3) or not frame.flags.c_contiguous:
            raise RuntimeError("decoded frame geometry or memory layout changed")
        try:
            self.process.stdin.write(memoryview(frame).cast("B"))
            self.process.stdin.flush()
        except BrokenPipeError as error:
            raise RuntimeError(self._ffmpeg_error("FFmpeg stopped while accepting a frame")) from error
        self.submitted += 1
        # Drain whatever FFmpeg has flushed without waiting. This avoids a
        # stdin/stdout pipe deadlock while keeping only a small compressed tail.
        return self._drain_available()

    @staticmethod
    def _take(output: queue.Queue[bytes | BaseException | None]) -> bytes:
        item = output.get(timeout=120)
        if isinstance(item, BaseException):
            raise RuntimeError("failed to read FFmpeg H.264 output") from item
        if item is None:
            raise RuntimeError("FFmpeg produced fewer H.264 frames than expected")
        return item

    def _take_pair(self) -> tuple[bytes, bytes]:
        left = self.left_pending.popleft() if self.left_pending else self._take(self.left_queue)
        right = self.right_pending.popleft() if self.right_pending else self._take(self.right_queue)
        self.received += 1
        return left, right

    def _drain_available(self) -> list[tuple[bytes, bytes]]:
        for source, pending in (
            (self.left_queue, self.left_pending),
            (self.right_queue, self.right_pending),
        ):
            while True:
                try:
                    item = source.get_nowait()
                except queue.Empty:
                    break
                if isinstance(item, BaseException):
                    raise RuntimeError("failed to read FFmpeg H.264 output") from item
                if item is not None:
                    pending.append(item)
        pairs: list[tuple[bytes, bytes]] = []
        while self.left_pending and self.right_pending:
            pairs.append((self.left_pending.popleft(), self.right_pending.popleft()))
            self.received += 1
        return pairs

    def finish(self) -> list[tuple[bytes, bytes]]:
        if self.closed:
            return []
        self.closed = True
        try:
            self.process.stdin.close()
            remaining: list[tuple[bytes, bytes]] = []
            while self.received < self.submitted:
                remaining.append(self._take_pair())
            return_code = self.process.wait(timeout=120)
            self.left_reader.join(timeout=5)
            self.right_reader.join(timeout=5)
            if return_code != 0:
                raise RuntimeError(self._ffmpeg_error(f"FFmpeg exited with status {return_code}"))
            return remaining
        except BaseException:
            self._terminate()
            raise
        finally:
            self._close_streams()

    def _ffmpeg_error(self, prefix: str) -> str:
        details = b""
        if self.process.stderr is not None:
            details = self.process.stderr.read()
        text = details.decode("utf-8", errors="replace").strip()
        return f"{prefix}: {text}" if text else prefix

    def abort(self) -> None:
        self.closed = True
        self._terminate()
        self._close_streams()

    def _terminate(self) -> None:
        try:
            if not self.process.stdin.closed:
                self.process.stdin.close()
        except (BrokenPipeError, OSError):
            pass
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait()

    def _close_streams(self) -> None:
        if not self.left_stream.closed:
            self.left_stream.close()
        if not self.right_stream.closed:
            self.right_stream.close()
        if self.process.stderr is not None and not self.process.stderr.closed:
            self.process.stderr.close()


class StereoSplitH264Converter:
    def __init__(self, config: ConverterConfig | None = None) -> None:
        self.config = config or ConverterConfig()
        self.typestore = get_typestore(Stores.ROS2_JAZZY)
        self.msg = self.typestore.types

    def convert(self, input_path: str | Path, output_path: str | Path) -> ConvertStats:
        config = self.config
        stats = ConvertStats()
        video_output_topics = {config.left_topic, config.right_topic}
        copied_topic_ids: set[int] = set()
        schema_ids: dict[int, int] = {}
        channel_ids: dict[int, int] = {}
        encoder: DualH264Encoder | None = None
        decoder: H264FrameDecoder | None = None
        video_metadata: deque[VideoMetadata] = deque()
        decoded_metadata: deque[VideoMetadata] = deque()

        with Path(input_path).open("rb") as source, Path(output_path).open("wb") as destination:
            reader = make_reader(source)
            summary = reader.get_summary()
            if summary:
                collisions = sorted(
                    channel.topic
                    for channel in summary.channels.values()
                    if channel.topic in video_output_topics
                )
                if collisions:
                    raise RuntimeError(f"input already contains output topic(s): {', '.join(collisions)}")
            input_format = self._detect_input_format(reader)
            source.seek(0)
            reader = make_reader(source)
            summary = reader.get_summary()
            source_has_imu = bool(
                summary
                and any(channel.topic == config.imu_topic for channel in summary.channels.values())
            )
            generate_imu = input_format in {"jpeg", "jpg"} and not source_has_imu

            writer = Writer(destination, compression=CompressionType.ZSTD)
            writer.start(profile="", library="archebase stereo-split-convert")
            video_schema_id = writer.register_schema(
                FOXGLOVE_SCHEMA_NAME, "protobuf", FOXGLOVE_DESCRIPTOR_SET
            )
            left_channel_id = writer.register_channel(config.left_topic, "protobuf", video_schema_id)
            right_channel_id = writer.register_channel(config.right_topic, "protobuf", video_schema_id)
            imu_channel_id: int | None = None
            if generate_imu:
                imu_definition, _ = self.typestore.generate_msgdef(
                    "sensor_msgs/msg/Imu", ros_version=2
                )
                imu_schema_id = writer.register_schema(
                    "sensor_msgs/msg/Imu", "ros2msg", imu_definition.encode("utf-8")
                )
                imu_channel_id = writer.register_channel(config.imu_topic, "cdr", imu_schema_id)
            ordered_writer = OrderedMessageWriter(writer)

            if summary:
                for channel in summary.channels.values():
                    if channel.topic == config.input_topic:
                        continue
                    schema = summary.schemas.get(channel.schema_id) if channel.schema_id else None
                    self._copy_channel(writer, schema, channel, schema_ids, channel_ids)
                    copied_topic_ids.add(channel.id)

            previous_image_timestamp: int | None = None

            def process_frame(frame: np.ndarray, metadata: VideoMetadata) -> None:
                nonlocal encoder, previous_image_timestamp
                required_width = config.metadata_width + config.eye_width * 2
                if frame.shape[0] < config.eye_height or frame.shape[1] < required_width:
                    raise RuntimeError(
                        f"frame {frame.shape[1]}x{frame.shape[0]} is smaller than "
                        f"required {required_width}x{config.eye_height}"
                    )
                if encoder is None:
                    encoder = DualH264Encoder(frame.shape[1], frame.shape[0], config)
                stats.decoded_images += 1
                video_metadata.append(metadata)
                for encoded in encoder.submit(frame):
                    self._write_video_pair(
                        ordered_writer, left_channel_id, right_channel_id,
                        video_metadata.popleft(), encoded, stats,
                    )

                imu = decode_imu_from_bgr(frame) if imu_channel_id is not None else None
                if imu is not None and previous_image_timestamp is not None:
                    timestamps = self._interpolate_imu_timestamps(
                        previous_image_timestamp, metadata.log_time, len(imu.samples)
                    )
                    for sample, timestamp in zip(imu.samples, timestamps):
                        imu_message = self._make_imu(sample, self._time_from_ns(timestamp))
                        ordered_writer.add_message(
                            imu_channel_id,
                            timestamp,
                            bytes(self.typestore.serialize_cdr(imu_message, "sensor_msgs/msg/Imu")),
                            timestamp,
                        )
                        stats.imu_messages += 1
                previous_image_timestamp = metadata.log_time

            try:
                for schema, channel, message in reader.iter_messages(log_time_order=True):
                    if channel.topic != config.input_topic:
                        if channel.topic in video_output_topics:
                            raise RuntimeError(f"input already contains output topic: {channel.topic}")
                        copied_channel = self._copy_channel(writer, schema, channel, schema_ids, channel_ids)
                        ordered_writer.add_message(
                            copied_channel,
                            message.log_time,
                            message.data,
                            message.publish_time,
                            message.sequence,
                        )
                        stats.copied_messages += 1
                        copied_topic_ids.add(channel.id)
                        continue

                    stats.input_messages += 1
                    if schema is None or schema.name != "sensor_msgs/msg/CompressedImage":
                        raise RuntimeError(
                            f"{config.input_topic} must use sensor_msgs/msg/CompressedImage"
                        )
                    compressed = self.typestore.deserialize_cdr(
                        message.data, "sensor_msgs/msg/CompressedImage"
                    )
                    stamp = compressed.header.stamp
                    metadata = VideoMetadata(
                        log_time=message.log_time,
                        publish_time=message.publish_time,
                        sequence=message.sequence,
                        seconds=int(stamp.sec),
                        nanos=int(stamp.nanosec),
                    )
                    compressed_format = self._normalize_compressed_format(compressed.format)
                    if compressed_format != input_format:
                        raise RuntimeError(
                            f"input cannot mix {input_format!r} and {compressed_format!r} frames"
                        )
                    if compressed_format == "h264":
                        if decoder is None:
                            access_unit = bytes(compressed.data)
                            if 5 not in h264_nal_types(access_unit):
                                stats.skipped_messages += 1
                                continue
                            decoder = H264FrameDecoder(
                                config.metadata_width + config.eye_width * 2,
                                config.eye_height,
                            )
                        decoded_metadata.append(metadata)
                        for frame in decoder.submit(bytes(compressed.data)):
                            process_frame(frame, decoded_metadata.popleft())
                    elif compressed_format in {"jpeg", "jpg"}:
                        if decoder is not None:
                            raise RuntimeError("input cannot mix H.264 and JPEG frames")
                        frame = cv2.imdecode(
                            np.asarray(compressed.data, dtype=np.uint8), cv2.IMREAD_COLOR
                        )
                        if frame is None or not frame.size:
                            stats.skipped_messages += 1
                            continue
                        process_frame(frame, metadata)
                    else:
                        raise RuntimeError(
                            f"unsupported CompressedImage format: {compressed.format!r}"
                        )

                    pending_video_times = [
                        pending[0].log_time
                        for pending in (decoded_metadata, video_metadata)
                        if pending
                    ]
                    if pending_video_times:
                        ordered_writer.flush_before(min(pending_video_times))
                    else:
                        ordered_writer.flush_before(message.log_time + 1)

                if stats.input_messages == 0:
                    raise RuntimeError(f"no CompressedImage topic found: {config.input_topic}")
                if decoder is not None:
                    for frame in decoder.finish():
                        process_frame(frame, decoded_metadata.popleft())
                    if decoded_metadata:
                        raise RuntimeError("H.264 decoder did not return every submitted frame")
                if encoder is None:
                    raise RuntimeError(f"no decodable images found: {config.input_topic}")
                for encoded in encoder.finish():
                    self._write_video_pair(
                        ordered_writer, left_channel_id, right_channel_id,
                        video_metadata.popleft(), encoded, stats,
                    )
                if video_metadata:
                    raise RuntimeError("H.264 encoder did not return every submitted frame")
                ordered_writer.flush_all()
                writer.finish()
            except BaseException:
                if decoder is not None:
                    decoder.abort()
                if encoder is not None:
                    encoder.abort()
                raise

        stats.copied_topics = len(copied_topic_ids)
        return stats

    def _detect_input_format(self, reader) -> str:
        for schema, _, message in reader.iter_messages(topics=[self.config.input_topic]):
            if schema is None or schema.name != "sensor_msgs/msg/CompressedImage":
                raise RuntimeError(
                    f"{self.config.input_topic} must use sensor_msgs/msg/CompressedImage"
                )
            compressed = self.typestore.deserialize_cdr(
                message.data, "sensor_msgs/msg/CompressedImage"
            )
            compressed_format = self._normalize_compressed_format(compressed.format)
            if compressed_format not in {"h264", "jpeg", "jpg"}:
                raise RuntimeError(
                    f"unsupported CompressedImage format: {compressed.format!r}"
                )
            return compressed_format
        raise RuntimeError(f"no CompressedImage topic found: {self.config.input_topic}")

    @staticmethod
    def _normalize_compressed_format(value: object) -> str:
        return str(value).lower().split(";", 1)[0].strip()

    @staticmethod
    def _copy_channel(writer, schema, channel, schema_ids, channel_ids) -> int:
        if channel.id in channel_ids:
            return channel_ids[channel.id]
        output_schema_id = 0
        if schema is not None:
            if schema.id not in schema_ids:
                schema_ids[schema.id] = writer.register_schema(schema.name, schema.encoding, schema.data)
            output_schema_id = schema_ids[schema.id]
        channel_ids[channel.id] = writer.register_channel(
            channel.topic, channel.message_encoding, output_schema_id, dict(channel.metadata)
        )
        return channel_ids[channel.id]

    def _write_video_pair(self, writer, left_channel, right_channel, metadata, encoded, stats) -> None:
        left_data, right_data = encoded
        for channel_id, frame_id, payload in (
            (left_channel, self.config.left_frame_id, left_data),
            (right_channel, self.config.right_frame_id, right_data),
        ):
            video = CompressedVideo()
            video.timestamp.seconds = metadata.seconds
            video.timestamp.nanos = metadata.nanos
            video.frame_id = frame_id
            video.data = payload
            video.format = OUTPUT_FORMAT
            writer.add_message(
                channel_id,
                metadata.log_time,
                video.SerializeToString(),
                metadata.publish_time,
                metadata.sequence,
            )
        stats.left_videos += 1
        stats.right_videos += 1

    def _make_time(self, sec: int, nanosec: int):
        return self.msg["builtin_interfaces/msg/Time"](sec=sec, nanosec=nanosec)

    def _time_from_ns(self, timestamp_ns: int):
        sec, nanosec = divmod(timestamp_ns, 1_000_000_000)
        return self._make_time(sec, nanosec)

    def _make_imu(self, sample: ImuSample, stamp):
        vector = self.msg["geometry_msgs/msg/Vector3"]
        quaternion = self.msg["geometry_msgs/msg/Quaternion"]
        header = self.msg["std_msgs/msg/Header"](stamp=stamp, frame_id=self.config.imu_frame_id)
        return self.msg["sensor_msgs/msg/Imu"](
            header=header,
            orientation=quaternion(x=0.0, y=0.0, z=0.0, w=1.0),
            orientation_covariance=np.array([-1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
            angular_velocity=vector(
                x=sample.gx_dps * np.pi / 180.0,
                y=sample.gy_dps * np.pi / 180.0,
                z=sample.gz_dps * np.pi / 180.0,
            ),
            angular_velocity_covariance=np.zeros(9, dtype=np.float64),
            linear_acceleration=vector(
                x=sample.ax_mg * 9.80665 / 1000.0,
                y=sample.ay_mg * 9.80665 / 1000.0,
                z=sample.az_mg * 9.80665 / 1000.0,
            ),
            linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
        )

    @staticmethod
    def _interpolate_imu_timestamps(start_ns: int, stop_ns: int, count: int) -> list[int]:
        if count <= 0:
            return []
        offset = (stop_ns - start_ns) // count
        return [start_ns + offset * (index + 1) for index in range(count)]


def stats_dict(stats: ConvertStats) -> dict[str, int]:
    return asdict(stats)
