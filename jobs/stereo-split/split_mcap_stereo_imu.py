#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Split DECXIN full-frame MJPG MCAP into stereo image and IMU topics."""

from __future__ import annotations

import argparse
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

import cv2
import numpy as np
from rosbags.highlevel import AnyReader
from rosbags.rosbag2 import StoragePlugin, Writer
from rosbags.typesys import Stores, get_typestore


K_GROUP_BYTES = 16
K_CODE_CELL = 8
K_LINE_SEARCH_OFFSET = 3
K_GRAVITY = 9.80665
K_PI = math.pi
DEVICE_ICM42688 = 1


@dataclass
class ImuSample:
    timestamp_us: int = 0
    ax_mg: float = 0.0
    ay_mg: float = 0.0
    az_mg: float = 0.0
    gx_dps: float = 0.0
    gy_dps: float = 0.0
    gz_dps: float = 0.0


@dataclass
class ImuFrame:
    protocol_type: int
    device_type: list[int]
    device_group_count: list[int]
    exposure_start_us: int
    exposure_end_us: int
    samples: list[ImuSample]


@dataclass
class SplitStats:
    input_messages: int = 0
    decoded_images: int = 0
    left_images: int = 0
    right_images: int = 0
    imu_messages: int = 0
    skipped_messages: int = 0


@dataclass
class DecxinMcapSplitterConfig:
    input_topic: str = "/decxin/rgb/compressed"
    left_topic: str = "/decxin/left_rgb"
    right_topic: str = "/decxin/right_rgb"
    imu_topic: str = "/decxin/imu"
    metadata_width: int = 160
    eye_width: int = 1920
    eye_height: int = 1200
    left_frame_id: str = "decxin_left_camera"
    right_frame_id: str = "decxin_right_camera"
    imu_frame_id: str = "decxin_imu"
    jpeg_quality: int = 95
    rotate_180: bool = True


class DecxinMcapStereoImuSplitter:
    """Convert full-frame DECXIN MJPG messages into stereo images and IMU MCAP."""

    def __init__(self, config: DecxinMcapSplitterConfig | None = None) -> None:
        self.config = config or DecxinMcapSplitterConfig()
        self.typestore = get_typestore(Stores.ROS2_JAZZY)
        self.msg = self.typestore.types

    def convert(self, input_path: str | Path, output_path: str | Path) -> SplitStats:
        """Read input MCAP/rosbag2 and write a new rosbag2 MCAP directory."""
        input_path = Path(input_path)
        output_path = Path(output_path)
        stats = SplitStats()

        with AnyReader([input_path], default_typestore=self.typestore) as reader:
            input_connections = [
                conn
                for conn in reader.connections
                if conn.topic == self.config.input_topic
                and conn.msgtype == "sensor_msgs/msg/CompressedImage"
            ]
            if not input_connections:
                raise RuntimeError(f"no CompressedImage topic found: {self.config.input_topic}")

            with Writer(output_path, version=9, storage_plugin=StoragePlugin.MCAP) as writer:
                left_conn = writer.add_connection(
                    self._compressed_topic(self.config.left_topic),
                    "sensor_msgs/msg/CompressedImage",
                    typestore=self.typestore,
                )
                right_conn = writer.add_connection(
                    self._compressed_topic(self.config.right_topic),
                    "sensor_msgs/msg/CompressedImage",
                    typestore=self.typestore,
                )
                imu_conn = writer.add_connection(
                    self.config.imu_topic, "sensor_msgs/msg/Imu", typestore=self.typestore
                )

                previous_image_timestamp: int | None = None
                for connection, timestamp, rawdata in reader.messages(input_connections):
                    stats.input_messages += 1
                    compressed = self.typestore.deserialize_cdr(rawdata, connection.msgtype)
                    bgr = self._decode_compressed_image(compressed.data)
                    if bgr is None:
                        stats.skipped_messages += 1
                        continue

                    stats.decoded_images += 1
                    left, right = self._split_stereo(bgr)
                    header_stamp = compressed.header.stamp

                    left_msg = self._make_compressed_image(left, header_stamp, self.config.left_frame_id)
                    right_msg = self._make_compressed_image(right, header_stamp, self.config.right_frame_id)
                    writer.write(
                        left_conn,
                        timestamp,
                        self.typestore.serialize_cdr(left_msg, "sensor_msgs/msg/CompressedImage"),
                    )
                    writer.write(
                        right_conn,
                        timestamp,
                        self.typestore.serialize_cdr(right_msg, "sensor_msgs/msg/CompressedImage"),
                    )
                    stats.left_images += 1
                    stats.right_images += 1

                    imu = self.decode_imu_from_bgr(bgr)
                    if imu and previous_image_timestamp is not None:
                        imu_timestamps = self._interpolate_imu_timestamps(
                            previous_image_timestamp, timestamp, len(imu.samples)
                        )
                        for sample, imu_timestamp in zip(imu.samples, imu_timestamps):
                            imu_msg = self._make_imu(sample, self._time_from_ns(imu_timestamp))
                            writer.write(
                                imu_conn,
                                imu_timestamp,
                                self.typestore.serialize_cdr(imu_msg, "sensor_msgs/msg/Imu"),
                            )
                            stats.imu_messages += 1

                    previous_image_timestamp = timestamp

        return stats

    def _decode_compressed_image(self, data: np.ndarray) -> np.ndarray | None:
        encoded = np.asarray(data, dtype=np.uint8)
        decoded = cv2.imdecode(encoded, cv2.IMREAD_COLOR)
        return decoded if decoded is not None and decoded.size else None

    def _split_stereo(self, bgr: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        cfg = self.config
        required_width = cfg.metadata_width + cfg.eye_width * 2
        if bgr.shape[1] < required_width or bgr.shape[0] < cfg.eye_height:
            raise RuntimeError(
                f"frame {bgr.shape[1]}x{bgr.shape[0]} is smaller than "
                f"stereo crop {required_width}x{cfg.eye_height}"
            )
        left = bgr[0 : cfg.eye_height, cfg.metadata_width : cfg.metadata_width + cfg.eye_width]
        right_start = cfg.metadata_width + cfg.eye_width
        right = bgr[0 : cfg.eye_height, right_start : right_start + cfg.eye_width]
        if cfg.rotate_180:
            left = cv2.rotate(left, cv2.ROTATE_180)
            right = cv2.rotate(right, cv2.ROTATE_180)
        return np.ascontiguousarray(left), np.ascontiguousarray(right)

    def _make_time(self, sec: int, nanosec: int):
        return self.msg["builtin_interfaces/msg/Time"](sec=sec, nanosec=nanosec)

    def _make_header(self, stamp, frame_id: str):
        return self.msg["std_msgs/msg/Header"](stamp=stamp, frame_id=frame_id)

    def _time_from_ns(self, timestamp_ns: int):
        sec, nanosec = divmod(timestamp_ns, 1_000_000_000)
        return self._make_time(sec, nanosec)

    @staticmethod
    def _compressed_topic(topic: str) -> str:
        return topic if topic.endswith("/compressed") else f"{topic}/compressed"

    def _make_compressed_image(self, bgr: np.ndarray, stamp, frame_id: str):
        encode_params = [int(cv2.IMWRITE_JPEG_QUALITY), self.config.jpeg_quality]
        ok, encoded = cv2.imencode(".jpg", bgr, encode_params)
        if not ok:
            raise RuntimeError("failed to encode split image as MJPG")
        return self.msg["sensor_msgs/msg/CompressedImage"](
            header=self._make_header(stamp, frame_id),
            format="jpeg",
            data=encoded.reshape(-1),
        )

    def _make_imu(self, sample: ImuSample, stamp):
        vector = self.msg["geometry_msgs/msg/Vector3"]
        quaternion = self.msg["geometry_msgs/msg/Quaternion"]
        return self.msg["sensor_msgs/msg/Imu"](
            header=self._make_header(stamp, self.config.imu_frame_id),
            orientation=quaternion(x=0.0, y=0.0, z=0.0, w=1.0),
            orientation_covariance=np.array([-1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
            angular_velocity=vector(
                x=sample.gx_dps * K_PI / 180.0,
                y=sample.gy_dps * K_PI / 180.0,
                z=sample.gz_dps * K_PI / 180.0,
            ),
            angular_velocity_covariance=np.zeros(9, dtype=np.float64),
            linear_acceleration=vector(
                x=sample.ax_mg * K_GRAVITY / 1000.0,
                y=sample.ay_mg * K_GRAVITY / 1000.0,
                z=sample.az_mg * K_GRAVITY / 1000.0,
            ),
            linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
        )

    @staticmethod
    def _interpolate_imu_timestamps(start_ns: int, stop_ns: int, sample_count: int) -> list[int]:
        if sample_count <= 0:
            return []
        offset = (stop_ns - start_ns) // sample_count
        return [start_ns + offset * (index + 1) for index in range(sample_count)]

    @staticmethod
    def decode_imu_from_bgr(bgr: np.ndarray) -> ImuFrame | None:
        candidates = (
            (K_LINE_SEARCH_OFFSET - 1, K_CODE_CELL),
            (bgr.shape[0] - K_LINE_SEARCH_OFFSET, -K_CODE_CELL),
            (K_LINE_SEARCH_OFFSET, K_CODE_CELL),
            (bgr.shape[0] - K_LINE_SEARCH_OFFSET - 1, -K_CODE_CELL),
        )
        for first_row, row_step in candidates:
            decoded = DecxinMcapStereoImuSplitter.decode_imu_from_rows(bgr, first_row, row_step)
            if decoded and decoded.samples:
                return decoded
        return None

    @staticmethod
    def decode_imu_from_rows(bgr: np.ndarray, first_row: int, row_step: int) -> ImuFrame | None:
        if bgr.ndim != 3 or bgr.shape[2] < 3:
            return None

        frame_bytes = bytearray()
        line_index = 0
        while line_index < K_GROUP_BYTES:
            row = first_row + line_index * row_step
            frame_bytes.extend(DecxinMcapStereoImuSplitter.decode_line(bgr, row))
            if len(frame_bytes) >= K_GROUP_BYTES:
                break
            line_index += 1

        if len(frame_bytes) < K_GROUP_BYTES:
            return None

        result = DecxinMcapStereoImuSplitter.decode_group_header(frame_bytes[:K_GROUP_BYTES])
        total_groups = 1 + sum(result.device_group_count)
        total_size = total_groups * K_GROUP_BYTES

        line_index += 1
        while len(frame_bytes) < total_size:
            row = first_row + line_index * row_step
            if row < 0 or row >= bgr.shape[0]:
                break
            frame_bytes.extend(DecxinMcapStereoImuSplitter.decode_line(bgr, row))
            line_index += 1

        if len(frame_bytes) < total_size:
            return None

        group_index = 1
        for device_type, count in zip(result.device_type, result.device_group_count):
            for i in range(count):
                group = bytes(frame_bytes[(group_index + i) * K_GROUP_BYTES : (group_index + i + 1) * K_GROUP_BYTES])
                if device_type == DEVICE_ICM42688:
                    result.samples.append(DecxinMcapStereoImuSplitter.decode_icm42688(group))
            group_index += count

        return result

    @staticmethod
    def decode_line(bgr: np.ndarray, row: int) -> bytes:
        if row < 0 or row >= bgr.shape[0]:
            return b""

        line = bgr[row]
        flat = line.reshape(-1)
        if flat[0] <= 220 or flat[1] <= 220 or flat[2] <= 220 or flat[3] <= 220:
            return b""

        x = 0
        while x < bgr.shape[1]:
            if int(line[x, 1]) < 220:
                x += K_CODE_CELL // 2
                break
            x += 1

        bits: list[int] = []
        while x + 1 < bgr.shape[1]:
            value = int(line[x, 1]) + int(line[x + 1, 1])
            if value < 100:
                bits.append(0)
            elif value < 440:
                bits.append(1)
            else:
                break
            x += K_CODE_CELL

        out = bytearray(len(bits) // 8)
        for i in range(len(out)):
            value = 0
            for bit in range(8):
                value |= bits[i * 8 + bit] << bit
            out[i] = value
        return bytes(out)

    @staticmethod
    def decode_group_header(data: bytes | bytearray) -> ImuFrame:
        if data[:8] == data[8:16]:
            return ImuFrame(
                protocol_type=0,
                device_type=[DEVICE_ICM42688, 0, 0, 0, 0],
                device_group_count=[11, 0, 0, 0, 0],
                exposure_start_us=DecxinMcapStereoImuSplitter.read_be_u32(data, 0),
                exposure_end_us=DecxinMcapStereoImuSplitter.read_be_u32(data, 4),
                samples=[],
            )

        return ImuFrame(
            protocol_type=data[0] >> 4,
            device_type=[
                ((data[0] & 0x0F) << 4) | ((data[1] & 0xF0) >> 4),
                data[2],
                ((data[3] & 0x0F) << 4) | ((data[4] & 0xF0) >> 4),
                data[5],
                ((data[6] & 0x0F) << 4) | ((data[7] & 0xF0) >> 4),
            ],
            device_group_count=[
                data[1] & 0x0F,
                data[3] >> 4,
                data[4] & 0x0F,
                data[6] >> 4,
                data[7] & 0x0F,
            ],
            exposure_start_us=DecxinMcapStereoImuSplitter.read_be_u32(data, 8),
            exposure_end_us=DecxinMcapStereoImuSplitter.read_be_u32(data, 12),
            samples=[],
        )

    @staticmethod
    def decode_icm42688(data: bytes | bytearray, acc_range: int = 0x02, gyro_range: int = 0x01) -> ImuSample:
        ax = DecxinMcapStereoImuSplitter.read_be_i16(data, 4)
        ay = DecxinMcapStereoImuSplitter.read_be_i16(data, 6)
        az = DecxinMcapStereoImuSplitter.read_be_i16(data, 8)
        gx = DecxinMcapStereoImuSplitter.read_be_i16(data, 10)
        gy = DecxinMcapStereoImuSplitter.read_be_i16(data, 12)
        gz = DecxinMcapStereoImuSplitter.read_be_i16(data, 14)

        sample = ImuSample(timestamp_us=DecxinMcapStereoImuSplitter.read_be_u32(data, 0))
        if (ax == -1 and ay == -1 and az == -1) or gx == -32768:
            return sample

        acc_scale = DecxinMcapStereoImuSplitter.acc_scale_mg(acc_range)
        gyro_scale = DecxinMcapStereoImuSplitter.gyro_scale_dps(gyro_range)
        sample.ax_mg = ax * acc_scale
        sample.ay_mg = ay * acc_scale
        sample.az_mg = az * acc_scale
        sample.gx_dps = gx * gyro_scale
        sample.gy_dps = gy * gyro_scale
        sample.gz_dps = gz * gyro_scale
        return sample

    @staticmethod
    def read_be_u32(data: bytes | bytearray, offset: int) -> int:
        return int.from_bytes(data[offset : offset + 4], byteorder="big", signed=False)

    @staticmethod
    def read_be_i16(data: bytes | bytearray, offset: int) -> int:
        return int.from_bytes(data[offset : offset + 2], byteorder="big", signed=True)

    @staticmethod
    def acc_scale_mg(scale: int) -> float:
        return {0x03: 2000.0, 0x02: 4000.0, 0x01: 8000.0, 0x00: 16000.0}.get(scale, 4000.0) / 32768.0

    @staticmethod
    def gyro_scale_dps(scale: int) -> float:
        return {
            0x00: 2000.0,
            0x01: 1000.0,
            0x02: 500.0,
            0x03: 250.0,
            0x04: 125.0,
            0x05: 62.5,
            0x06: 31.25,
            0x07: 15.125,
        }.get(scale, 1000.0) / 32768.0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Split /decxin/rgb/compressed full-frame DECXIN MCAP into left/right image and IMU topics.",
    )
    parser.add_argument("input", type=Path, help="Input MCAP file or rosbag2 directory.")
    parser.add_argument("output", type=Path, help="Output rosbag2 directory using MCAP storage.")
    parser.add_argument("--input-topic", default="/decxin/rgb/compressed")
    parser.add_argument("--left-topic", default="/decxin/left_rgb")
    parser.add_argument("--right-topic", default="/decxin/right_rgb")
    parser.add_argument("--imu-topic", default="/decxin/imu")
    parser.add_argument("--metadata-width", type=int, default=160)
    parser.add_argument("--eye-width", type=int, default=1920)
    parser.add_argument("--eye-height", type=int, default=1200)
    parser.add_argument("--left-frame-id", default="decxin_left_camera")
    parser.add_argument("--right-frame-id", default="decxin_right_camera")
    parser.add_argument("--imu-frame-id", default="decxin_imu")
    parser.add_argument("--jpeg-quality", type=int, default=95, help="JPEG quality used for split image topics.")
    parser.add_argument(
        "--no-rotate-180",
        action="store_true",
        help="Keep the cropped left/right images in their original orientation.",
    )
    return parser


def main(argv: Iterable[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    config = DecxinMcapSplitterConfig(
        input_topic=args.input_topic,
        left_topic=args.left_topic,
        right_topic=args.right_topic,
        imu_topic=args.imu_topic,
        metadata_width=args.metadata_width,
        eye_width=args.eye_width,
        eye_height=args.eye_height,
        left_frame_id=args.left_frame_id,
        right_frame_id=args.right_frame_id,
        imu_frame_id=args.imu_frame_id,
        jpeg_quality=args.jpeg_quality,
        rotate_180=not args.no_rotate_180,
    )
    stats = DecxinMcapStereoImuSplitter(config).convert(args.input, args.output)
    print(
        "converted "
        f"input={stats.input_messages} decoded={stats.decoded_images} "
        f"left={stats.left_images} right={stats.right_images} "
        f"imu={stats.imu_messages} skipped={stats.skipped_messages}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
