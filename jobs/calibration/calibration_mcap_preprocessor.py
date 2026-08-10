#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Create a calibration-ready stereo and IMU MCAP from a joined DECXIN capture."""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO
import queue
import re
import subprocess
import threading

import cv2
import numpy as np
from rosbags.highlevel import AnyReader
from rosbags.rosbag2 import StoragePlugin, Writer
from rosbags.typesys import Stores, get_typestore

from split_mcap_stereo_imu import DecxinMcapStereoImuSplitter, SplitInputRejected


ANNEX_B_START_CODE_PATTERN = re.compile(b"(?:\\x00\\x00\\x00\\x01|\\x00\\x00\\x01)")


@dataclass(frozen=True)
class CalibrationPreprocessorConfig:
    input_topic: str = "/decxin/rgb/compressed"
    left_topic: str = "/decxin/left_rgb"
    right_topic: str = "/decxin/right_rgb"
    imu_topic: str = "/decxin/imu"
    metadata_width: int = 160
    eye_width: int = 1920
    eye_height: int = 1200
    left_frame_id: str = "decxin_left_camera"
    right_frame_id: str = "decxin_right_camera"
    jpeg_quality: int = 95


@dataclass
class CalibrationPreprocessStats:
    source_codec: str = ""
    imu_source: str = ""
    input_messages: int = 0
    decoded_images: int = 0
    left_images: int = 0
    right_images: int = 0
    imu_messages: int = 0
    skipped_messages: int = 0


@dataclass(frozen=True)
class FrameMetadata:
    timestamp: int
    stamp: object


class CalibrationInputRejected(RuntimeError):
    """The source capture cannot be normalized for calibration."""


def compressed_image_codec(format_value: str) -> str:
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
        raise CalibrationInputRejected(
            f"unsupported CompressedImage format: {format_value!r}"
        )
    return codecs.pop()


def annex_b_nal_units(data: bytes) -> list[bytes]:
    starts = list(ANNEX_B_START_CODE_PATTERN.finditer(data))
    units = []
    for index, start in enumerate(starts):
        end = starts[index + 1].start() if index + 1 < len(starts) else len(data)
        if start.end() < end:
            units.append(data[start.end() : end])
    return units


def rbsp_from_ebsp(data: bytes) -> bytes:
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


def read_unsigned_exp_golomb(data: bytes) -> int:
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


def h264_picture_count(data: bytes) -> int:
    pictures = 0
    nal_units = annex_b_nal_units(data)
    if not nal_units:
        return 0
    for nal_unit in nal_units:
        nal_type = nal_unit[0] & 0x1F
        if nal_type not in {1, 2, 5, 19, 20, 21}:
            continue
        slice_offset = 4 if nal_type in {20, 21} else 1
        if len(nal_unit) <= slice_offset:
            raise ValueError("truncated H.264 slice")
        first_macroblock = read_unsigned_exp_golomb(
            rbsp_from_ebsp(nal_unit[slice_offset:])
        )
        if first_macroblock == 0:
            pictures += 1
    return pictures


def validate_single_h264_picture(data: bytes) -> None:
    try:
        picture_count = h264_picture_count(data)
    except ValueError as error:
        raise CalibrationInputRejected(
            "H.264 input must contain one decoded frame per input message: "
            "invalid Annex-B slice data"
        ) from error
    if picture_count != 1:
        raise CalibrationInputRejected(
            "H.264 input must contain one decoded frame per input message: "
            f"found {picture_count} encoded pictures"
        )


def h264_nal_types(data: bytes) -> set[int]:
    return {nal_unit[0] & 0x1F for nal_unit in annex_b_nal_units(data)}


class RawVideoFrameReader(threading.Thread):
    """Read fixed-size BGR frames without blocking the FFmpeg stdout pipe."""

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
                                "FFmpeg returned a truncated raw frame: "
                                f"{offset}/{self.frame_bytes} bytes"
                            )
                        return
                    offset += size
                self.output.put(bytes(frame))
        except BaseException as error:
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
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-nostdin",
                "-f",
                "h264",
                "-i",
                "pipe:0",
                "-map",
                "0:v:0",
                "-vf",
                f"crop={width}:{height}:0:0",
                "-fps_mode",
                "passthrough",
                "-pix_fmt",
                "bgr24",
                "-f",
                "rawvideo",
                "pipe:1",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        if self.process.stdin is None or self.process.stdout is None:
            raise RuntimeError("FFmpeg H.264 decoder pipes were not created")
        self.reader = RawVideoFrameReader(
            self.process.stdout, self.frame_bytes, self.output
        )
        self.reader.start()

    def submit(self, access_unit: bytes) -> list[np.ndarray]:
        if self.closed:
            raise RuntimeError("cannot submit an H.264 frame after decoder close")
        try:
            self.process.stdin.write(access_unit)
            self.process.stdin.flush()
        except BrokenPipeError as error:
            raise RuntimeError(
                self.ffmpeg_error("FFmpeg stopped while accepting H.264")
            ) from error
        self.submitted += 1
        return self.drain_available()

    def as_frame(self, data: bytes) -> np.ndarray:
        self.received += 1
        if self.received > self.submitted:
            raise self.frame_count_error()
        return np.frombuffer(data, dtype=np.uint8).reshape(
            self.height, self.width, 3
        )

    def drain_available(self) -> list[np.ndarray]:
        frames = []
        while True:
            try:
                item = self.output.get_nowait()
            except queue.Empty:
                return frames
            if isinstance(item, BaseException):
                raise RuntimeError("failed to read FFmpeg decoded video") from item
            if item is None:
                raise self.decoder_stopped_error()
            frames.append(self.as_frame(item))

    def decoder_stopped_error(self) -> RuntimeError:
        return_code = self.process.poll()
        if return_code is None:
            try:
                return_code = self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                return RuntimeError("FFmpeg decoded fewer H.264 frames than expected")
        if return_code != 0:
            return RuntimeError(
                self.ffmpeg_error(
                    f"FFmpeg could not decode required {self.width}x{self.height} H.264 frames"
                )
            )
        return RuntimeError("FFmpeg H.264 decoder stopped before input completed")

    def frame_count_error(self) -> RuntimeError:
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
            remaining = []
            while True:
                try:
                    item = self.output.get(timeout=120)
                except queue.Empty as error:
                    raise RuntimeError(
                        "timed out waiting for FFmpeg decoded video"
                    ) from error
                if isinstance(item, BaseException):
                    raise RuntimeError("failed to read FFmpeg decoded video") from item
                if item is None:
                    break
                remaining.append(self.as_frame(item))
            return_code = self.process.wait(timeout=120)
            self.reader.join(timeout=5)
            if return_code != 0:
                raise RuntimeError(
                    self.ffmpeg_error(
                        f"FFmpeg could not decode required {self.width}x{self.height} H.264 frames"
                    )
                )
            if self.received != self.submitted:
                raise self.frame_count_error()
            return remaining
        except BaseException:
            self.terminate()
            raise
        finally:
            self.close_streams()

    def abort(self) -> None:
        self.closed = True
        self.terminate()
        self.close_streams()

    def ffmpeg_error(self, prefix: str) -> str:
        details = b""
        if self.process.stderr is not None:
            details = self.process.stderr.read()
        text = details.decode("utf-8", errors="replace").strip()
        return f"{prefix}: {text}" if text else prefix

    def terminate(self) -> None:
        try:
            if self.process.stdin is not None and not self.process.stdin.closed:
                self.process.stdin.close()
        except (BrokenPipeError, OSError):
            pass
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait()

    def close_streams(self) -> None:
        if self.process.stdout is not None and not self.process.stdout.closed:
            self.process.stdout.close()
        if self.process.stderr is not None and not self.process.stderr.closed:
            self.process.stderr.close()


class DecxinCalibrationMcapPreprocessor:
    """Normalize JPEG or H.264 joined captures for the calibration CLI."""

    def __init__(self, config: CalibrationPreprocessorConfig | None = None) -> None:
        self.config = config or CalibrationPreprocessorConfig()
        self.typestore = get_typestore(Stores.ROS2_JAZZY)
        self.msg = self.typestore.types

    def convert(self, input_path: Path, output_path: Path) -> CalibrationPreprocessStats:
        codec = self.detect_source_codec(input_path)
        if codec == "jpeg":
            return self.convert_jpeg(input_path, output_path)
        return self.convert_h264(input_path, output_path)

    def detect_source_codec(self, input_path: Path) -> str:
        with AnyReader([input_path], default_typestore=self.typestore) as reader:
            connections = [
                connection
                for connection in reader.connections
                if connection.topic == self.config.input_topic
                and connection.msgtype == "sensor_msgs/msg/CompressedImage"
            ]
            detected_codec: str | None = None
            for connection, _, rawdata in reader.messages(connections=connections):
                message = self.typestore.deserialize_cdr(rawdata, connection.msgtype)
                codec = compressed_image_codec(message.format)
                if detected_codec is None:
                    detected_codec = codec
                    # The H.264 converter checks every message while decoding.
                    # The legacy JPEG splitter does not inspect the format field,
                    # so scan JPEG input here before delegating to it.
                    if detected_codec == "h264":
                        return detected_codec
                elif codec != detected_codec:
                    raise CalibrationInputRejected(
                        "input cannot mix JPEG and H.264 frames"
                    )
            if detected_codec is not None:
                return detected_codec
        raise CalibrationInputRejected(
            f"no CompressedImage topic found: {self.config.input_topic}"
        )

    def convert_jpeg(
        self, input_path: Path, output_path: Path
    ) -> CalibrationPreprocessStats:
        try:
            original = DecxinMcapStereoImuSplitter().convert(input_path, output_path)
        except SplitInputRejected as error:
            raise CalibrationInputRejected(str(error)) from error
        return CalibrationPreprocessStats(
            source_codec="jpeg",
            imu_source="embedded_metadata",
            input_messages=original.input_messages,
            decoded_images=original.decoded_images,
            left_images=original.left_images,
            right_images=original.right_images,
            imu_messages=original.imu_messages,
            skipped_messages=original.skipped_messages,
        )

    def convert_h264(
        self, input_path: Path, output_path: Path
    ) -> CalibrationPreprocessStats:
        config = self.config
        stats = CalibrationPreprocessStats(
            source_codec="h264", imu_source="existing_topic"
        )
        decoder: H264FrameDecoder | None = None
        pending_metadata: deque[FrameMetadata] = deque()
        with AnyReader([input_path], default_typestore=self.typestore) as reader:
            input_connections = [
                connection
                for connection in reader.connections
                if connection.topic == config.input_topic
                and connection.msgtype == "sensor_msgs/msg/CompressedImage"
            ]
            imu_connections = [
                connection
                for connection in reader.connections
                if connection.topic == config.imu_topic
                and connection.msgtype == "sensor_msgs/msg/Imu"
            ]
            if not input_connections:
                raise CalibrationInputRejected(
                    f"no CompressedImage topic found: {config.input_topic}"
                )
            if not imu_connections or next(
                reader.messages(connections=imu_connections), None
            ) is None:
                raise CalibrationInputRejected(
                    f"H.264 input requires source IMU topic: {config.imu_topic}"
                )

            with Writer(output_path, version=9, storage_plugin=StoragePlugin.MCAP) as writer:
                left_connection = writer.add_connection(
                    self.compressed_topic(config.left_topic),
                    "sensor_msgs/msg/CompressedImage",
                    typestore=self.typestore,
                )
                right_connection = writer.add_connection(
                    self.compressed_topic(config.right_topic),
                    "sensor_msgs/msg/CompressedImage",
                    typestore=self.typestore,
                )
                imu_connection = writer.add_connection(
                    config.imu_topic, "sensor_msgs/msg/Imu", typestore=self.typestore
                )

                def write_frame(frame: np.ndarray, metadata: FrameMetadata) -> None:
                    stats.decoded_images += 1
                    left, right = self.split_stereo(frame)
                    writer.write(
                        left_connection,
                        metadata.timestamp,
                        self.typestore.serialize_cdr(
                            self.make_compressed_image(
                                left, metadata.stamp, config.left_frame_id
                            ),
                            "sensor_msgs/msg/CompressedImage",
                        ),
                    )
                    writer.write(
                        right_connection,
                        metadata.timestamp,
                        self.typestore.serialize_cdr(
                            self.make_compressed_image(
                                right, metadata.stamp, config.right_frame_id
                            ),
                            "sensor_msgs/msg/CompressedImage",
                        ),
                    )
                    stats.left_images += 1
                    stats.right_images += 1

                try:
                    connections = input_connections + imu_connections
                    for connection, timestamp, rawdata in reader.messages(
                        connections=connections
                    ):
                        if connection.topic == config.imu_topic:
                            writer.write(imu_connection, timestamp, rawdata)
                            stats.imu_messages += 1
                            continue

                        stats.input_messages += 1
                        message = self.typestore.deserialize_cdr(
                            rawdata, connection.msgtype
                        )
                        if compressed_image_codec(message.format) != "h264":
                            raise CalibrationInputRejected(
                                "input cannot mix JPEG and H.264 frames"
                            )
                        access_unit = bytes(message.data)
                        validate_single_h264_picture(access_unit)
                        if decoder is None and 5 not in h264_nal_types(access_unit):
                            stats.skipped_messages += 1
                            continue
                        if decoder is None:
                            decoder = H264FrameDecoder(
                                config.metadata_width + config.eye_width * 2,
                                config.eye_height,
                            )
                        pending_metadata.append(
                            FrameMetadata(timestamp=timestamp, stamp=message.header.stamp)
                        )
                        for frame in decoder.submit(access_unit):
                            write_frame(frame, pending_metadata.popleft())

                    if decoder is None:
                        raise CalibrationInputRejected(
                            f"no decodable images found: {config.input_topic}"
                        )
                    for frame in decoder.finish():
                        write_frame(frame, pending_metadata.popleft())
                    if pending_metadata:
                        raise RuntimeError(
                            "H.264 decoder did not return every submitted frame"
                        )
                except BaseException:
                    if decoder is not None and not decoder.closed:
                        decoder.abort()
                    raise
        return stats

    def split_stereo(self, frame: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        config = self.config
        required_width = config.metadata_width + config.eye_width * 2
        if frame.shape[1] < required_width or frame.shape[0] < config.eye_height:
            raise CalibrationInputRejected(
                f"frame {frame.shape[1]}x{frame.shape[0]} is smaller than "
                f"stereo crop {required_width}x{config.eye_height}"
            )
        left = frame[
            0 : config.eye_height,
            config.metadata_width : config.metadata_width + config.eye_width,
        ]
        right_start = config.metadata_width + config.eye_width
        right = frame[
            0 : config.eye_height,
            right_start : right_start + config.eye_width,
        ]
        return np.ascontiguousarray(left), np.ascontiguousarray(right)

    @staticmethod
    def compressed_topic(topic: str) -> str:
        return topic if topic.endswith("/compressed") else f"{topic}/compressed"

    def make_compressed_image(self, frame: np.ndarray, stamp: object, frame_id: str):
        parameters = [int(cv2.IMWRITE_JPEG_QUALITY), self.config.jpeg_quality]
        ok, encoded = cv2.imencode(".jpg", frame, parameters)
        if not ok:
            raise RuntimeError("failed to encode split image as JPEG")
        header = self.msg["std_msgs/msg/Header"](stamp=stamp, frame_id=frame_id)
        return self.msg["sensor_msgs/msg/CompressedImage"](
            header=header, format="jpeg", data=encoded.reshape(-1)
        )
