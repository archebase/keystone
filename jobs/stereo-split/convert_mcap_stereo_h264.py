#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Convert one DECXIN joined image topic to stereo H.264 and IMU MCAP topics."""

from __future__ import annotations

from bisect import bisect_right
from collections import deque
from dataclasses import asdict, dataclass
import heapq
import os
from pathlib import Path
import queue
import re
import statistics
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
ANNEX_B_START_CODE_PATTERN = re.compile(b"\\x00\\x00(?:\\x00)?\\x01")


def _compressed_image_codec(format_value: str) -> str:
    normalized = str(format_value).lower().strip()
    if not normalized:
        return "jpeg"
    tokens = set(re.findall(r"[a-z0-9.]+", normalized))
    codecs = set()
    if tokens.intersection({"jpeg", "jpg"}):
        codecs.add("jpeg")
    if tokens.intersection({"h264", "h.264"}):
        codecs.add("h264")
    if len(codecs) != 1:
        raise RuntimeError(f"unsupported CompressedImage format: {format_value!r}")
    return codecs.pop()


def _annex_b_nal_units(data: bytes) -> list[bytes]:
    starts = list(ANNEX_B_START_CODE_PATTERN.finditer(data))
    nal_units = []
    for index, start in enumerate(starts):
        end = starts[index + 1].start() if index + 1 < len(starts) else len(data)
        if start.end() < end:
            nal_units.append(data[start.end() : end])
    return nal_units


def _rbsp_from_ebsp(data: bytes) -> bytes:
    rbsp = bytearray()
    offset = 0
    while offset < len(data):
        if data[offset : offset + 3] == b"\x00\x00\x03":
            rbsp.extend(b"\x00\x00")
            offset += 3
            continue
        rbsp.append(data[offset])
        offset += 1
    return bytes(rbsp)


def _read_unsigned_exp_golomb(data: bytes) -> int:
    bit_count = len(data) * 8
    leading_zeroes = 0
    while leading_zeroes < bit_count:
        if data[leading_zeroes // 8] & (1 << (7 - leading_zeroes % 8)):
            break
        leading_zeroes += 1
    if leading_zeroes == bit_count or leading_zeroes * 2 + 1 > bit_count:
        raise ValueError("truncated Exp-Golomb value")
    value = (1 << leading_zeroes) - 1
    for offset in range(leading_zeroes):
        bit_index = leading_zeroes + 1 + offset
        if data[bit_index // 8] & (1 << (7 - bit_index % 8)):
            value += 1 << (leading_zeroes - 1 - offset)
    return value


def _h264_picture_count(data: bytes) -> int:
    pictures = 0
    nal_units = _annex_b_nal_units(data)
    if not nal_units:
        return 0
    for nal_unit in nal_units:
        nal_type = nal_unit[0] & 0x1F
        if nal_type not in {1, 2, 5, 19, 20, 21}:
            continue
        slice_offset = 4 if nal_type in {20, 21} else 1
        if len(nal_unit) <= slice_offset:
            raise ValueError("truncated H.264 slice")
        first_macroblock = _read_unsigned_exp_golomb(
            _rbsp_from_ebsp(nal_unit[slice_offset:])
        )
        if first_macroblock == 0:
            pictures += 1
    return pictures


def _validate_single_h264_picture(data: bytes) -> None:
    try:
        picture_count = _h264_picture_count(data)
    except ValueError as error:
        raise RuntimeError(
            "H.264 input must contain one decoded frame per input message: "
            "invalid Annex-B slice data"
        ) from error
    if picture_count != 1:
        raise RuntimeError(
            "H.264 input must contain one decoded frame per input message: "
            f"found {picture_count} encoded pictures"
        )


def _h264_nal_types(data: bytes) -> set[int]:
    return {nal_unit[0] & 0x1F for nal_unit in _annex_b_nal_units(data)}


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
    timestamp_repair_applied: bool = False
    timestamp_repair_reason: str = "not_evaluated"
    timestamp_log_repair_applied: bool = False
    timestamp_publish_repair_applied: bool = False
    timestamp_repaired_messages: int = 0


@dataclass(frozen=True)
class VideoMetadata:
    log_time: int
    publish_time: int
    sequence: int
    seconds: int
    nanos: int


@dataclass(frozen=True)
class _TimestampSample:
    log_time: int
    publish_time: int
    header_time: int


@dataclass(frozen=True)
class _TimestampAxisRepair:
    applied: bool = False
    source_times: tuple[int, ...] = ()
    repaired_times: tuple[int, ...] = ()
    interpolate: bool = True
    source_anchor: int = 0
    header_anchor: int = 0

    def video_time(self, timestamp: int, header_time: int) -> int:
        if not self.applied:
            return timestamp
        return self.source_anchor + header_time - self.header_anchor

    def message_time(self, timestamp: int, sample_index: int) -> int:
        if not self.applied:
            return timestamp
        if not self.interpolate:
            index = min(max(sample_index, 0), len(self.source_times) - 1)
            return self.repaired_times[index] + timestamp - self.source_times[index]
        return self._interpolate_time(timestamp)

    def _interpolate_time(self, timestamp: int) -> int:
        index = bisect_right(self.source_times, timestamp) - 1
        if index < 0:
            return self.repaired_times[0] + timestamp - self.source_times[0]
        if index >= len(self.source_times) - 1:
            return self.repaired_times[-1] + timestamp - self.source_times[-1]
        source_start = self.source_times[index]
        source_span = self.source_times[index + 1] - source_start
        repaired_start = self.repaired_times[index]
        repaired_span = self.repaired_times[index + 1] - repaired_start
        return repaired_start + (timestamp - source_start) * repaired_span // source_span

@dataclass(frozen=True)
class _TimestampRepairPlan:
    samples: tuple[_TimestampSample, ...] = ()
    log_repair: _TimestampAxisRepair = _TimestampAxisRepair()
    publish_repair: _TimestampAxisRepair = _TimestampAxisRepair()
    reason: str = "not_evaluated"

    @property
    def applied(self) -> bool:
        return self.log_repair.applied or self.publish_repair.applied

    @property
    def log_repair_applied(self) -> bool:
        return self.log_repair.applied

    @property
    def publish_repair_applied(self) -> bool:
        return self.publish_repair.applied

    @property
    def requires_physical_order(self) -> bool:
        return any(
            repair.applied and not repair.interpolate
            for repair in (self.log_repair, self.publish_repair)
        )

    def video_message_times(self, sample: _TimestampSample) -> tuple[int, int]:
        return (
            self.log_repair.video_time(sample.log_time, sample.header_time),
            self.publish_repair.video_time(sample.publish_time, sample.header_time),
        )

    def new_cursor(self) -> _TimestampRepairCursor:
        return _TimestampRepairCursor(self)


class _TimestampRepairCursor:
    """Map copied messages against the preceding video in physical MCAP order."""

    def __init__(self, plan: _TimestampRepairPlan) -> None:
        self.plan = plan
        self.sample_index = -1

    def message_times(self, log_time: int, publish_time: int) -> tuple[int, int]:
        return (
            self.plan.log_repair.message_time(log_time, self.sample_index),
            self.plan.publish_repair.message_time(publish_time, self.sample_index),
        )

    def video_message_times(self, sample: _TimestampSample) -> tuple[int, int]:
        self.sample_index += 1
        return self.plan.video_message_times(sample)


_MIN_TIMESTAMP_REPAIR_SAMPLES = 30
_MIN_HEADER_GAP_NS = 1_000_000
_MAX_HEADER_GAP_NS = 1_000_000_000
_MIN_STALL_GAP_NS = 250_000_000
_TIMESTAMP_REPAIR_WINDOW_GAPS = 300
_MIN_BURST_GAP_RATIO = 0.2


def _percentile(values: list[int], fraction: float) -> int:
    ordered = sorted(values)
    index = round((len(ordered) - 1) * fraction)
    return ordered[index]


def _contains_bursty_window(
    gaps: list[int], burst_limit: int, stall_limit: int
) -> bool:
    window_size = min(len(gaps), _TIMESTAMP_REPAIR_WINDOW_GAPS)
    burst_flags = [gap < burst_limit for gap in gaps]
    stall_flags = [gap >= stall_limit for gap in gaps]
    burst_count = sum(burst_flags[:window_size])
    stall_count = sum(stall_flags[:window_size])
    for start in range(len(gaps) - window_size + 1):
        if burst_count / window_size >= _MIN_BURST_GAP_RATIO and stall_count > 0:
            return True
        if start + window_size == len(gaps):
            break
        burst_count += burst_flags[start + window_size] - burst_flags[start]
        stall_count += stall_flags[start + window_size] - stall_flags[start]
    return False


def _timestamp_axis_issue(
    times: list[int], header_times: list[int], median_header_gap: int
) -> str | None:
    if all(timestamp == 0 for timestamp in times):
        return None
    gaps = [current - previous for previous, current in zip(times, times[1:])]
    if any(gap <= 0 for gap in gaps):
        return "non_monotonic"

    header_span = header_times[-1] - header_times[0]
    span_ratio = (times[-1] - times[0]) / header_span
    if not 0.5 <= span_ratio <= 2.0:
        return "rate_mismatch"

    burst_limit = median_header_gap // 2
    stall_limit = max(_MIN_STALL_GAP_NS, median_header_gap * 8)
    if _contains_bursty_window(gaps, burst_limit, stall_limit):
        return "bursty"
    return None


def _make_timestamp_axis_repair(
    times: list[int], header_times: list[int], issue: str | None
) -> _TimestampAxisRepair:
    if issue is None:
        return _TimestampAxisRepair()

    repaired_times = [
        times[0] + header_time - header_times[0] for header_time in header_times
    ]
    strictly_increasing = all(
        current > previous for previous, current in zip(times, times[1:])
    )
    return _TimestampAxisRepair(
        applied=True,
        source_times=tuple(times),
        repaired_times=tuple(repaired_times),
        interpolate=strictly_increasing,
        source_anchor=times[0],
        header_anchor=header_times[0],
    )


def _build_timestamp_repair_plan(samples: list[_TimestampSample]) -> _TimestampRepairPlan:
    """Repair queue-burst timing only when the embedded sensor clock is trustworthy."""
    sample_tuple = tuple(samples)
    if len(samples) < _MIN_TIMESTAMP_REPAIR_SAMPLES:
        return _TimestampRepairPlan(samples=sample_tuple, reason="insufficient_samples")

    header_times = [sample.header_time for sample in samples]
    header_gaps = [
        current.header_time - previous.header_time
        for previous, current in zip(samples, samples[1:])
    ]
    if any(sample.header_time <= 0 for sample in samples):
        return _TimestampRepairPlan(samples=sample_tuple, reason="header_missing")
    if any(gap <= 0 for gap in header_gaps):
        return _TimestampRepairPlan(samples=sample_tuple, reason="header_non_monotonic")

    median_header_gap = int(statistics.median(header_gaps))
    header_p01 = _percentile(header_gaps, 0.01)
    header_p99 = _percentile(header_gaps, 0.99)
    header_is_healthy = (
        _MIN_HEADER_GAP_NS <= median_header_gap <= _MAX_HEADER_GAP_NS
        and header_p01 >= max(_MIN_HEADER_GAP_NS, median_header_gap // 4)
        and header_p99 <= max(median_header_gap * 3, median_header_gap + 20_000_000)
    )
    if not header_is_healthy:
        return _TimestampRepairPlan(samples=sample_tuple, reason="header_unstable")

    log_times = [sample.log_time for sample in samples]
    publish_times = [sample.publish_time for sample in samples]
    log_issue = _timestamp_axis_issue(log_times, header_times, median_header_gap)
    publish_issue = _timestamp_axis_issue(publish_times, header_times, median_header_gap)
    reasons = [
        f"{axis}_{issue}"
        for axis, issue in (("log", log_issue), ("publish", publish_issue))
        if issue is not None
    ]
    if not reasons:
        return _TimestampRepairPlan(
            samples=sample_tuple, reason="outer_timestamps_healthy"
        )

    return _TimestampRepairPlan(
        samples=sample_tuple,
        log_repair=_make_timestamp_axis_repair(log_times, header_times, log_issue),
        publish_repair=_make_timestamp_axis_repair(
            publish_times, header_times, publish_issue
        ),
        reason=",".join(reasons),
    )


class OrderedMessageWriter:
    """Buffer delayed output and write MCAP messages in log-time order."""

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
    """Decode an Annex-B H.264 stream to fixed-size BGR frames."""

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
                "-map", "0:v:0", "-vf", f"crop={width}:{height}:0:0",
                "-fps_mode", "passthrough", "-pix_fmt", "bgr24",
                "-f", "rawvideo", "pipe:1",
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
        if self.received > self.submitted:
            raise self._frame_count_error()
        return np.frombuffer(data, dtype=np.uint8).reshape(self.height, self.width, 3)

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
                raise self._decoder_stopped_error()
            frames.append(self._as_frame(item))

    def _decoder_stopped_error(self) -> RuntimeError:
        return_code = self.process.poll()
        if return_code is None:
            try:
                return_code = self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                return RuntimeError("FFmpeg decoded fewer H.264 frames than expected")
        if return_code != 0:
            return RuntimeError(
                self._ffmpeg_error(
                    f"FFmpeg could not decode required {self.width}x{self.height} H.264 frames"
                )
            )
        return RuntimeError("FFmpeg H.264 decoder stopped before input completed")

    def _frame_count_error(self) -> RuntimeError:
        return RuntimeError(
            "H.264 input must contain one decoded frame per input message: "
            f"received {self.submitted} messages and decoded {self.received} frames"
        )

    def finish(self) -> list[np.ndarray]:
        if self.closed:
            return []
        self.closed = True
        try:
            self.process.stdin.close()
            remaining: list[np.ndarray] = []
            while True:
                try:
                    item = self.output.get(timeout=120)
                except queue.Empty as error:
                    raise RuntimeError("timed out waiting for FFmpeg decoded video") from error
                if isinstance(item, BaseException):
                    raise RuntimeError("failed to read FFmpeg decoded video") from item
                if item is None:
                    break
                remaining.append(self._as_frame(item))
            return_code = self.process.wait(timeout=120)
            self.reader.join(timeout=5)
            if return_code != 0:
                raise RuntimeError(
                    self._ffmpeg_error(
                        f"FFmpeg could not decode required {self.width}x{self.height} H.264 frames"
                    )
                )
            if self.received != self.submitted:
                raise self._frame_count_error()
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

    def __init__(
        self,
        width: int,
        height: int,
        config: ConverterConfig,
        rotate_180: bool,
    ) -> None:
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
        orientation_filter = ",hflip,vflip" if rotate_180 else ""
        filter_graph = (
            "[0:v]split=2[left_input][right_input];"
            f"[left_input]crop={config.eye_width}:{config.eye_height}:"
            f"{config.metadata_width}:0{orientation_filter},format=yuv420p[left];"
            f"[right_input]crop={config.eye_width}:{config.eye_height}:"
            f"{config.metadata_width + config.eye_width}:0"
            f"{orientation_filter},format=yuv420p[right]"
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

    def convert(
        self,
        input_path: str | Path,
        output_path: str | Path,
        calibration_attachment: bytes | None = None,
    ) -> ConvertStats:
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
            input_codec = self._detect_input_codec(reader)
            source.seek(0)
            timestamp_repair = self._timestamp_repair_plan(make_reader(source))
            stats.timestamp_repair_applied = timestamp_repair.applied
            stats.timestamp_repair_reason = timestamp_repair.reason
            stats.timestamp_log_repair_applied = timestamp_repair.log_repair_applied
            stats.timestamp_publish_repair_applied = (
                timestamp_repair.publish_repair_applied
            )
            source_imu_channel_ids = (
                {
                    channel.id
                    for channel in summary.channels.values()
                    if channel.topic == config.imu_topic
                }
                if summary
                else set()
            )
            source_imu_messages: int | None = None
            if summary is not None and summary.statistics is not None:
                source_imu_messages = sum(
                    summary.statistics.channel_message_counts.get(channel_id, 0)
                    for channel_id in source_imu_channel_ids
                )
            if source_imu_messages is None:
                source.seek(0)
                probe_reader = make_reader(source)
                source_imu_messages = int(
                    next(
                        probe_reader.iter_messages(topics=[config.imu_topic]), None
                    ) is not None
                )
            if source_imu_channel_ids and source_imu_messages == 0:
                raise RuntimeError(
                    f"source IMU topic contains no messages: {config.imu_topic}"
                )
            # H.264 compression is lossy, so embedded pixel IMU metadata is
            # not reliable after decode. Preserve the source IMU instead.
            if input_codec == "h264" and source_imu_messages == 0:
                raise RuntimeError(
                    f"H.264 input requires source IMU topic: {config.imu_topic}"
                )
            generate_imu = input_codec == "jpeg" and source_imu_messages == 0
            source.seek(0)
            reader = make_reader(source)

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
                # Register before copying records so even empty source
                # channels survive the conversion.
                for channel in summary.channels.values():
                    if channel.topic == config.input_topic:
                        continue
                    schema = summary.schemas.get(channel.schema_id) if channel.schema_id else None
                    self._copy_channel(writer, schema, channel, schema_ids, channel_ids)
                    copied_topic_ids.add(channel.id)

            previous_image_timestamp: int | None = None
            timestamp_cursor = timestamp_repair.new_cursor()

            def process_frame(frame: np.ndarray, metadata: VideoMetadata) -> None:
                nonlocal encoder, previous_image_timestamp
                required_width = config.metadata_width + config.eye_width * 2
                if frame.shape[0] < config.eye_height or frame.shape[1] < required_width:
                    raise RuntimeError(
                        f"frame {frame.shape[1]}x{frame.shape[0]} is smaller than "
                        f"required {required_width}x{config.eye_height}"
                    )
                if encoder is None:
                    # JPEG captures are upside down; H.264 captures already have final orientation.
                    encoder = DualH264Encoder(
                        frame.shape[1],
                        frame.shape[0],
                        config,
                        rotate_180=input_codec == "jpeg",
                    )
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
                for schema, channel, message in reader.iter_messages(
                    log_time_order=not timestamp_repair.requires_physical_order
                ):
                    if channel.topic != config.input_topic:
                        if channel.topic in video_output_topics:
                            raise RuntimeError(f"input already contains output topic: {channel.topic}")
                        copied_channel = self._copy_channel(writer, schema, channel, schema_ids, channel_ids)
                        log_time, publish_time = timestamp_cursor.message_times(
                            message.log_time, message.publish_time
                        )
                        ordered_writer.add_message(
                            copied_channel,
                            log_time,
                            message.data,
                            publish_time,
                            message.sequence,
                        )
                        stats.copied_messages += 1
                        if timestamp_repair.applied:
                            stats.timestamp_repaired_messages += 1
                        copied_topic_ids.add(channel.id)
                        if channel.topic == config.imu_topic:
                            stats.imu_messages += 1
                        continue

                    stats.input_messages += 1
                    if timestamp_repair.applied:
                        stats.timestamp_repaired_messages += 1
                    if schema is None or schema.name != "sensor_msgs/msg/CompressedImage":
                        raise RuntimeError(
                            f"{config.input_topic} must use sensor_msgs/msg/CompressedImage"
                        )
                    compressed = self.typestore.deserialize_cdr(
                        message.data, "sensor_msgs/msg/CompressedImage"
                    )
                    stamp = compressed.header.stamp
                    timestamp_sample = _TimestampSample(
                        log_time=message.log_time,
                        publish_time=message.publish_time,
                        header_time=int(stamp.sec) * 1_000_000_000 + int(stamp.nanosec),
                    )
                    log_time, publish_time = timestamp_cursor.video_message_times(
                        timestamp_sample
                    )
                    metadata = VideoMetadata(
                        log_time=log_time,
                        publish_time=publish_time,
                        sequence=message.sequence,
                        seconds=int(stamp.sec),
                        nanos=int(stamp.nanosec),
                    )
                    message_codec = _compressed_image_codec(compressed.format)
                    if input_codec != message_codec:
                        raise RuntimeError("input cannot mix JPEG and H.264 frames")
                    if message_codec == "h264":
                        access_unit = bytes(compressed.data)
                        _validate_single_h264_picture(access_unit)
                        if decoder is None and 5 not in _h264_nal_types(access_unit):
                            stats.skipped_messages += 1
                            ordered_writer.flush_before(log_time + 1)
                            continue
                        if decoder is None:
                            decoder = H264FrameDecoder(
                                config.metadata_width + config.eye_width * 2,
                                config.eye_height,
                            )
                        decoded_metadata.append(metadata)
                        for frame in decoder.submit(access_unit):
                            process_frame(frame, decoded_metadata.popleft())
                    else:
                        frame = cv2.imdecode(
                            np.asarray(compressed.data, dtype=np.uint8), cv2.IMREAD_COLOR
                        )
                        if frame is None or not frame.size:
                            stats.skipped_messages += 1
                            continue
                        process_frame(frame, metadata)

                    pending_video_times = [
                        pending[0].log_time
                        for pending in (decoded_metadata, video_metadata)
                        if pending
                    ]
                    if pending_video_times:
                        ordered_writer.flush_before(min(pending_video_times))
                    else:
                        ordered_writer.flush_before(log_time + 1)

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
                if calibration_attachment is not None:
                    writer.add_attachment(
                        create_time=0,
                        log_time=0,
                        name="archebase/calibration/result.json",
                        media_type="application/json",
                        data=calibration_attachment,
                    )
                writer.finish()
            except BaseException:
                if decoder is not None:
                    decoder.abort()
                if encoder is not None:
                    encoder.abort()
                raise

        stats.copied_topics = len(copied_topic_ids)
        return stats

    def _detect_input_codec(self, reader) -> str:
        for schema, _, message in reader.iter_messages(topics=[self.config.input_topic]):
            if schema is None or schema.name != "sensor_msgs/msg/CompressedImage":
                raise RuntimeError(
                    f"{self.config.input_topic} must use sensor_msgs/msg/CompressedImage"
                )
            compressed = self.typestore.deserialize_cdr(
                message.data, "sensor_msgs/msg/CompressedImage"
            )
            return _compressed_image_codec(compressed.format)
        raise RuntimeError(f"no CompressedImage topic found: {self.config.input_topic}")

    def _timestamp_repair_plan(self, reader) -> _TimestampRepairPlan:
        samples = []
        for schema, _, message in reader.iter_messages(
            topics=[self.config.input_topic], log_time_order=False
        ):
            if schema is None or schema.name != "sensor_msgs/msg/CompressedImage":
                return _TimestampRepairPlan()
            compressed = self.typestore.deserialize_cdr(
                message.data, "sensor_msgs/msg/CompressedImage"
            )
            stamp = compressed.header.stamp
            samples.append(
                _TimestampSample(
                    log_time=message.log_time,
                    publish_time=message.publish_time,
                    header_time=int(stamp.sec) * 1_000_000_000 + int(stamp.nanosec),
                )
            )
        return _build_timestamp_repair_plan(samples)

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


def stats_dict(stats: ConvertStats) -> dict[str, int | bool | str]:
    return asdict(stats)
