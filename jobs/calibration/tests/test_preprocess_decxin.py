# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

from pathlib import Path
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


def make_source(
    path: Path,
    frame_count: int = 2,
    topic: str = "/decxin/rgb/compressed",
) -> Path:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    messages = typestore.types
    with Writer(path, version=9, storage_plugin=StoragePlugin.MCAP) as writer:
        connection = writer.add_connection(
            topic,
            "sensor_msgs/msg/CompressedImage",
            typestore=typestore,
        )
        for index in range(frame_count):
            ok, jpeg = cv2.imencode(
                ".jpg", contract_frame(index), [int(cv2.IMWRITE_JPEG_QUALITY), 100]
            )
            if not ok:
                raise RuntimeError("failed to create JPEG fixture")
            stamp = messages["builtin_interfaces/msg/Time"](sec=index + 1, nanosec=0)
            header = messages["std_msgs/msg/Header"](stamp=stamp, frame_id="joined_camera")
            compressed = messages["sensor_msgs/msg/CompressedImage"](
                header=header, format="jpeg", data=jpeg.reshape(-1)
            )
            writer.write(
                connection,
                (index + 1) * 1_000_000_000,
                typestore.serialize_cdr(compressed, "sensor_msgs/msg/CompressedImage"),
            )
    return next(path.glob("*.mcap"))


class PreprocessDecxinTest(unittest.TestCase):
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
