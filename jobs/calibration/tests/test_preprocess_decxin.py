# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

import cv2
import numpy as np
from rosbags.highlevel import AnyReader
from rosbags.rosbag2 import StoragePlugin, Writer
from rosbags.typesys import Stores, get_typestore


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
from preprocess_decxin import PreprocessingRejected, preprocess_decxin  # noqa: E402
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
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-f",
            "rawvideo",
            "-pixel_format",
            "bgr24",
            "-video_size",
            f"{width}x{height}",
            "-framerate",
            "60",
            "-i",
            "pipe:0",
            "-c:v",
            "libx264",
            "-preset",
            "ultrafast",
            "-tune",
            "zerolatency",
            "-profile:v",
            "high",
            "-pix_fmt",
            "yuv420p",
            "-g",
            "1",
            "-bf",
            "0",
            "-x264-params",
            "aud=1:repeat-headers=1",
            "-f",
            "h264",
            "pipe:1",
        ],
        input=b"".join(frame.tobytes() for frame in frames),
        capture_output=True,
        check=True,
    ).stdout
    starts = [
        match.start()
        for match in re.finditer(b"\\x00\\x00(?:\\x00)?\\x01\\x09", encoded)
    ]
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
    frame_count: int = 2,
    topic: str = "/decxin/rgb/compressed",
    *,
    source_format: str = "jpeg",
    source_formats: list[str] | None = None,
    source_frames: list[np.ndarray] | None = None,
    source_payloads: list[bytes] | None = None,
    include_existing_imu: bool = False,
    include_empty_imu: bool = False,
) -> Path:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    messages = typestore.types
    if source_payloads is not None:
        frame_count = len(source_payloads)
    elif source_frames is not None:
        frame_count = len(source_frames)
    frames = source_frames or [contract_frame(index) for index in range(frame_count)]
    if len(frames) != frame_count:
        raise ValueError("source frame count must match payload count")
    frame_count = len(frames)
    formats = source_formats or [source_format] * frame_count
    if len(formats) != frame_count:
        raise ValueError("source format count must match frame count")
    h264_access_units = (
        encode_h264_access_units(frames)
        if source_payloads is None and "h264" in formats
        else []
    )
    with Writer(path, version=9, storage_plugin=StoragePlugin.MCAP) as writer:
        connection = writer.add_connection(
            topic,
            "sensor_msgs/msg/CompressedImage",
            typestore=typestore,
        )
        imu_connection = (
            writer.add_connection(
                "/decxin/imu",
                "sensor_msgs/msg/Imu",
                typestore=typestore,
            )
            if include_existing_imu or include_empty_imu
            else None
        )
        for index in range(frame_count):
            message_format = formats[index]
            if source_payloads is not None:
                payload = np.frombuffer(source_payloads[index], dtype=np.uint8)
            elif message_format == "h264":
                payload = np.frombuffer(h264_access_units[index], dtype=np.uint8)
            else:
                ok, payload = cv2.imencode(
                    ".jpg", frames[index], [int(cv2.IMWRITE_JPEG_QUALITY), 100]
                )
                if not ok:
                    raise RuntimeError("failed to create JPEG fixture")
            stamp = messages["builtin_interfaces/msg/Time"](sec=index + 1, nanosec=0)
            header = messages["std_msgs/msg/Header"](stamp=stamp, frame_id="joined_camera")
            compressed = messages["sensor_msgs/msg/CompressedImage"](
                header=header, format=message_format, data=payload.reshape(-1)
            )
            timestamp = (index + 1) * 1_000_000_000
            writer.write(
                connection,
                timestamp,
                typestore.serialize_cdr(compressed, "sensor_msgs/msg/CompressedImage"),
            )
            if imu_connection is not None and include_existing_imu:
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
                writer.write(
                    imu_connection,
                    timestamp + 1,
                    typestore.serialize_cdr(imu, "sensor_msgs/msg/Imu"),
                )
    return next(path.glob("*.mcap"))


def output_image(path: Path, topic: str) -> np.ndarray:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    with AnyReader([path], default_typestore=typestore) as reader:
        connections = [connection for connection in reader.connections if connection.topic == topic]
        connection, _, rawdata = next(reader.messages(connections=connections))
    message = typestore.deserialize_cdr(rawdata, connection.msgtype)
    frame = cv2.imdecode(np.asarray(message.data, dtype=np.uint8), cv2.IMREAD_COLOR)
    if frame is None:
        raise RuntimeError(f"failed to decode output fixture topic: {topic}")
    return frame


class PreprocessDecxinTest(unittest.TestCase):
    def test_writes_calibration_ready_mcap_from_joined_h264_and_source_imu(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(
                root / "source",
                source_format="h264",
                include_existing_imu=True,
            )

            output, stats = preprocess_decxin(source, root / "preprocessed")

            self.assertEqual(stats["source_codec"], "h264")
            self.assertEqual(stats["imu_source"], "existing_topic")
            self.assertEqual(stats["decoded_images"], 2)
            self.assertEqual(stats["imu_messages"], 2)
            typestore = get_typestore(Stores.ROS2_JAZZY)
            formats: dict[str, list[str]] = {}
            imu_frame_ids = []
            with AnyReader([output], default_typestore=typestore) as reader:
                topics = {connection.topic for connection in reader.connections}
                for connection, _, rawdata in reader.messages():
                    if connection.msgtype == "sensor_msgs/msg/Imu":
                        message = typestore.deserialize_cdr(rawdata, connection.msgtype)
                        imu_frame_ids.append(message.header.frame_id)
                    elif connection.msgtype != "sensor_msgs/msg/CompressedImage":
                        continue
                    else:
                        message = typestore.deserialize_cdr(rawdata, connection.msgtype)
                        formats.setdefault(connection.topic, []).append(message.format)
            self.assertEqual(
                topics,
                {"/decxin/left_rgb/compressed", "/decxin/right_rgb/compressed", "/decxin/imu"},
            )
            self.assertEqual(formats["/decxin/left_rgb/compressed"], ["jpeg", "jpeg"])
            self.assertEqual(formats["/decxin/right_rgb/compressed"], ["jpeg", "jpeg"])
            self.assertEqual(imu_frame_ids, ["source_imu", "source_imu"])

    def test_h264_input_preserves_existing_eye_orientation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(
                root / "source",
                source_format="h264",
                source_frames=[orientation_frame()],
                include_existing_imu=True,
            )

            output, _ = preprocess_decxin(source, root / "preprocessed")

            left = output_image(output, "/decxin/left_rgb/compressed")
            self.assertGreater(left[:200, :200].mean(), left[-200:, -200:].mean() + 100)
            right = output_image(output, "/decxin/right_rgb/compressed")
            self.assertGreater(right[:200, -200:].mean(), right[-200:, :200].mean() + 100)

    def test_rejects_h264_input_without_source_imu(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(root / "source", source_format="h264")

            with self.assertRaisesRegex(
                PreprocessingRejected, "H.264 input requires source IMU topic"
            ):
                preprocess_decxin(source, root / "preprocessed")

    def test_rejects_h264_input_with_empty_source_imu(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(
                root / "source",
                source_format="h264",
                include_empty_imu=True,
            )

            with self.assertRaisesRegex(
                PreprocessingRejected, "H.264 input requires source IMU topic"
            ):
                preprocess_decxin(source, root / "preprocessed")

    def test_rejects_h264_then_jpeg_input(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(
                root / "source",
                source_formats=["h264", "jpeg"],
                include_existing_imu=True,
            )

            with self.assertRaisesRegex(
                PreprocessingRejected, "input cannot mix JPEG and H.264 frames"
            ):
                preprocess_decxin(source, root / "preprocessed")

    def test_rejects_jpeg_then_h264_input(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(
                root / "source",
                source_formats=["jpeg", "h264"],
            )

            with self.assertRaisesRegex(
                PreprocessingRejected, "input cannot mix JPEG and H.264 frames"
            ):
                preprocess_decxin(source, root / "preprocessed")

    def test_skips_h264_frames_before_first_idr(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            idr = encode_h264_access_units([contract_frame(0)])[0]
            non_idr = b"\x00\x00\x00\x01\x09\x30\x00\x00\x00\x01\x41\x80"
            source = make_source(
                root / "source",
                source_format="h264",
                source_payloads=[non_idr, idr],
                include_existing_imu=True,
            )

            _, stats = preprocess_decxin(source, root / "preprocessed")

            self.assertEqual(stats["input_messages"], 2)
            self.assertEqual(stats["decoded_images"], 1)
            self.assertEqual(stats["skipped_messages"], 1)

    def test_rejects_multiple_h264_frames_in_one_input_message(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            access_units = encode_h264_access_units(
                [contract_frame(0), contract_frame(1)]
            )
            source = make_source(
                root / "source",
                source_format="h264",
                source_payloads=[b"".join(access_units)],
                include_existing_imu=True,
            )

            with self.assertRaisesRegex(
                PreprocessingRejected,
                "H.264 input must contain one decoded frame per input message",
            ):
                preprocess_decxin(source, root / "preprocessed")

    def test_writes_calibration_ready_stereo_and_imu_mcap(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(root / "source")

            output, stats = preprocess_decxin(source, root / "preprocessed")

            self.assertTrue(output.is_file())
            self.assertEqual(stats["decoded_images"], 2)
            self.assertEqual(stats["left_images"], 2)
            self.assertEqual(stats["right_images"], 2)
            self.assertEqual(stats["imu_messages"], 11)
            typestore = get_typestore(Stores.ROS2_JAZZY)
            with AnyReader([output], default_typestore=typestore) as reader:
                topics = {connection.topic for connection in reader.connections}
                messages = list(reader.messages())
            self.assertEqual(
                topics,
                {"/decxin/left_rgb/compressed", "/decxin/right_rgb/compressed", "/decxin/imu"},
            )
            self.assertEqual(len(messages), 15)

    def test_rejects_capture_without_extractable_imu_samples(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(root / "source", frame_count=1)

            with self.assertRaisesRegex(PreprocessingRejected, "did not produce IMU samples"):
                preprocess_decxin(source, root / "preprocessed")

    def test_rejects_capture_without_expected_stereo_topic(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = make_source(
                root / "source", frame_count=0, topic="/unexpected/topic"
            )

            with self.assertRaisesRegex(PreprocessingRejected, "no CompressedImage topic found"):
                preprocess_decxin(source, root / "preprocessed")


if __name__ == "__main__":
    unittest.main()
