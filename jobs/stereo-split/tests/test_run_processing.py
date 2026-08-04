# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import cv2
from mcap.reader import make_reader
import numpy as np
from rosbags.rosbag2 import StoragePlugin, Writer
from rosbags.typesys import Stores, get_typestore


JOB_ROOT = Path(__file__).resolve().parents[1]
RUNNER = JOB_ROOT / "run_processing.py"
MCAP_MAGIC = b"\x89MCAP0\r\n"
sys.path.insert(0, str(JOB_ROOT))
import run_processing as processing_runner  # noqa: E402
from convert_mcap_stereo_h264 import CompressedVideo, FOXGLOVE_SCHEMA_NAME  # noqa: E402
sys.path.remove(str(JOB_ROOT))


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def encode_metadata_line(frame: np.ndarray, row: int, payload: bytes) -> None:
    if len(payload) != 16:
        raise ValueError("metadata groups must contain 16 bytes")

    frame[row, :, :] = 255
    frame[row, 4:8, :] = 0
    for bit_index in range(128):
        bit = (payload[bit_index // 8] >> (bit_index % 8)) & 1
        cell_start = 8 + bit_index * 8
        frame[row, cell_start : cell_start + 8, :] = 100 if bit else 0


def make_contract_frame() -> np.ndarray:
    frame = np.zeros((1200, 4000, 3), dtype=np.uint8)
    frame[:, 160:2080, :] = (10, 80, 150)
    frame[:, 2080:4000, :] = (120, 30, 200)
    frame[200:400, 160:360, :] = (200, 20, 20)

    exposure = (123).to_bytes(4, "big") + (456).to_bytes(4, "big")
    groups = [exposure + exposure]
    for index in range(11):
        groups.append(
            (index + 1).to_bytes(4, "big")
            + (1000).to_bytes(2, "big", signed=True)
            + (-2000).to_bytes(2, "big", signed=True)
            + (3000).to_bytes(2, "big", signed=True)
            + (4000).to_bytes(2, "big", signed=True)
            + (-5000).to_bytes(2, "big", signed=True)
            + (6000).to_bytes(2, "big", signed=True)
        )
    for index, group in enumerate(groups):
        encode_metadata_line(frame, 2 + index * 8, group)
    return frame


def make_source_mcap(root: Path, topic: str = "/decxin/rgb/compressed") -> Path:
    typestore = get_typestore(Stores.ROS2_JAZZY)
    messages = typestore.types
    bag_dir = root / "source_bag"

    frame = make_contract_frame()
    ok, encoded = cv2.imencode(
        ".jpg",
        frame,
        [int(cv2.IMWRITE_JPEG_QUALITY), 100],
    )
    if not ok:
        raise RuntimeError("failed to encode test frame")

    with Writer(bag_dir, version=9, storage_plugin=StoragePlugin.MCAP) as writer:
        connection = writer.add_connection(
            topic,
            "sensor_msgs/msg/CompressedImage",
            typestore=typestore,
        )
        for index in range(2):
            timestamp = (index + 1) * 1_000_000_000
            stamp = messages["builtin_interfaces/msg/Time"](
                sec=index + 1,
                nanosec=0,
            )
            header = messages["std_msgs/msg/Header"](
                stamp=stamp,
                frame_id="decxin_test",
            )
            message = messages["sensor_msgs/msg/CompressedImage"](
                header=header,
                format="jpeg",
                data=encoded.reshape(-1),
            )
            writer.write(
                connection,
                timestamp,
                typestore.serialize_cdr(message, "sensor_msgs/msg/CompressedImage"),
            )

    source = root / "source.mcap"
    source.write_bytes(next(bag_dir.glob("*.mcap")).read_bytes())
    return source


def job_command(
    source: Path,
    output_binding: Path,
    scratch: Path,
    *,
    expected_size: int | None = None,
    expected_checksum: str | None = None,
    runner: Path = RUNNER,
) -> list[str]:
    return [
        sys.executable,
        str(runner),
        "--input",
        str(source),
        "--output-binding",
        str(output_binding),
        "--scratch",
        str(scratch),
        "--expected-source-size",
        str(expected_size if expected_size is not None else source.stat().st_size),
        "--expected-source-checksum",
        expected_checksum if expected_checksum is not None else sha256(source),
        "--source-uri",
        "tos://test-bucket/raw/source.mcap",
        "--processor-image",
        "ghcr.io/archebase/stereo-split@sha256:test",
        "--kind",
        "stereo_split",
        "--generation",
        "1",
    ]


def run_job(
    source: Path,
    output_binding: Path,
    scratch: Path,
    *,
    expected_size: int | None = None,
    expected_checksum: str | None = None,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        job_command(
            source,
            output_binding,
            scratch,
            expected_size=expected_size,
            expected_checksum=expected_checksum,
        ),
        cwd=JOB_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )


class RunProcessingTest(unittest.TestCase):
    def test_copy_retries_a_transient_failure(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source"
            destination = root / "destination"
            source.write_bytes(b"retryable copy")
            original_copy = processing_runner.copy_and_hash
            attempts = 0

            def flaky_copy(source_path: Path, destination_path: Path):
                nonlocal attempts
                attempts += 1
                if attempts == 1:
                    raise OSError("transient read error")
                return original_copy(source_path, destination_path)

            with (
                mock.patch.object(processing_runner, "copy_and_hash", side_effect=flaky_copy),
                mock.patch.object(processing_runner.time, "sleep"),
            ):
                identity = processing_runner.copy_and_hash_with_retries(source, destination)

            self.assertEqual(attempts, 2)
            self.assertEqual(identity.sha256, sha256(source))
            self.assertEqual(destination.read_bytes(), source.read_bytes())

    def test_publishes_outputs_and_manifest_for_valid_source(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = make_source_mcap(root)
            source_checksum = sha256(source)
            source.chmod(0o444)
            output_binding = root / "published"
            scratch = root / "scratch"
            output_binding.mkdir()
            (output_binding / "output_bag.mcap").write_bytes(b"partial")
            (output_binding / "metadata.yaml").write_bytes(b"partial")

            result = run_job(source, output_binding, scratch)

            self.assertEqual(result.returncode, 0, result.stderr)
            output_mcap = output_binding / "output_bag.mcap"
            output_metadata = output_binding / "metadata.yaml"
            manifest_path = output_binding / "processing_manifest.json"
            self.assertGreater(output_mcap.stat().st_size, 0)
            self.assertGreater(output_metadata.stat().st_size, 0)
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            metadata = json.loads(output_metadata.read_text(encoding="utf-8"))
            self.assertEqual(manifest["schema_version"], 2)
            self.assertEqual(manifest["status"], "succeeded")
            self.assertEqual(manifest["kind"], "stereo_split")
            self.assertEqual(manifest["output_format"], "stereo_h264")
            self.assertEqual(manifest["generation"], 1)
            self.assertEqual(manifest["source"]["sha256"], source_checksum)
            self.assertEqual(sha256(source), source_checksum)
            self.assertEqual(source.stat().st_mode & 0o222, 0)
            self.assertEqual(manifest["outputs"]["mcap"]["sha256"], sha256(output_mcap))
            self.assertEqual(manifest["stats"]["input_messages"], 2)
            self.assertEqual(manifest["stats"]["left_videos"], 2)
            self.assertEqual(manifest["stats"]["right_videos"], 2)
            self.assertEqual(manifest["stats"]["imu_messages"], 11)
            self.assertEqual(metadata["video"]["codec"], "h264")
            self.assertGreaterEqual(
                manifest_path.stat().st_mtime_ns,
                max(output_mcap.stat().st_mtime_ns, output_metadata.stat().st_mtime_ns),
            )
            self.assertFalse((scratch / "input" / ".source.mcap.partial").exists())

            records_by_topic: dict[str, list[tuple[object, object, object]]] = {}
            with output_mcap.open("rb") as stream:
                reader = make_reader(stream)
                for schema, channel, message in reader.iter_messages():
                    records_by_topic.setdefault(channel.topic, []).append(
                        (schema, channel, message)
                    )

            self.assertEqual(
                set(records_by_topic),
                {
                    "/decxin/left_rgb/h264",
                    "/decxin/right_rgb/h264",
                    "/decxin/imu",
                },
            )
            left_records = records_by_topic["/decxin/left_rgb/h264"]
            right_records = records_by_topic["/decxin/right_rgb/h264"]
            imu_records = records_by_topic["/decxin/imu"]
            expected_video_times = [1_000_000_000, 2_000_000_000]
            self.assertEqual([item[2].log_time for item in left_records], expected_video_times)
            self.assertEqual([item[2].log_time for item in right_records], expected_video_times)
            for records in (left_records, right_records):
                for schema, channel, message in records:
                    self.assertEqual(schema.name, FOXGLOVE_SCHEMA_NAME)
                    self.assertEqual(channel.message_encoding, "protobuf")
                    video = CompressedVideo.FromString(message.data)
                    self.assertEqual(video.format, "h264")
                    self.assertTrue(video.data)

            self.assertEqual(len(imu_records), 11)
            typestore = get_typestore(Stores.ROS2_JAZZY)
            first_imu_record = imu_records[0][2]
            first_imu_timestamp = first_imu_record.log_time
            first_imu = typestore.deserialize_cdr(first_imu_record.data, "sensor_msgs/msg/Imu")
            expected_imu_timestamp = 1_000_000_000 + 1_000_000_000 // 11
            self.assertEqual(first_imu_timestamp, expected_imu_timestamp)
            self.assertEqual(first_imu.header.stamp.sec, 1)
            self.assertEqual(first_imu.header.stamp.nanosec, expected_imu_timestamp % 1_000_000_000)
            acceleration_scale = 4000.0 / 32768.0 * 9.80665 / 1000.0
            gyro_scale = 1000.0 / 32768.0 * math.pi / 180.0
            self.assertAlmostEqual(first_imu.linear_acceleration.x, 1000 * acceleration_scale)
            self.assertAlmostEqual(first_imu.linear_acceleration.y, -2000 * acceleration_scale)
            self.assertAlmostEqual(first_imu.linear_acceleration.z, 3000 * acceleration_scale)
            self.assertAlmostEqual(first_imu.angular_velocity.x, 4000 * gyro_scale)
            self.assertAlmostEqual(first_imu.angular_velocity.y, -5000 * gyro_scale)
            self.assertAlmostEqual(first_imu.angular_velocity.z, 6000 * gyro_scale)

    def test_same_source_produces_deterministic_outputs(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = make_source_mcap(root)
            checksums = []

            for index in range(2):
                output_binding = root / f"published-{index}"
                output_binding.mkdir()
                result = run_job(source, output_binding, root / f"scratch-{index}")
                self.assertEqual(result.returncode, 0, result.stderr)
                checksums.append(
                    (
                        sha256(output_binding / "output_bag.mcap"),
                        sha256(output_binding / "metadata.yaml"),
                    )
                )

            self.assertEqual(checksums[0], checksums[1])

    def test_rejects_source_size_mismatch_without_publishing_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = make_source_mcap(root)
            output_binding = root / "published"
            output_binding.mkdir()

            result = run_job(
                source,
                output_binding,
                root / "scratch",
                expected_size=source.stat().st_size + 1,
            )

            self.assertEqual(result.returncode, 1)
            self.assertIn("source size mismatch", result.stderr)
            self.assertFalse((output_binding / "processing_manifest.json").exists())

    def test_rejects_insufficient_scratch_before_copying_input(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            source.write_bytes(MCAP_MAGIC + MCAP_MAGIC)
            output_binding = root / "published"
            output_binding.mkdir()
            scratch = root / "scratch"

            result = run_job(
                source,
                output_binding,
                scratch,
                expected_size=10**18,
            )

            self.assertEqual(result.returncode, 1)
            self.assertIn("insufficient scratch space", result.stderr)
            self.assertFalse((scratch / "input" / "source.mcap").exists())
            self.assertFalse((output_binding / "processing_manifest.json").exists())

    def test_rejects_source_checksum_mismatch_without_publishing_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = make_source_mcap(root)
            output_binding = root / "published"
            output_binding.mkdir()

            result = run_job(
                source,
                output_binding,
                root / "scratch",
                expected_checksum="0" * 64,
            )

            self.assertEqual(result.returncode, 1)
            self.assertIn("source checksum mismatch", result.stderr)
            self.assertFalse((output_binding / "processing_manifest.json").exists())

    def test_split_failure_does_not_publish_success_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = make_source_mcap(root, topic="/unexpected/topic")
            output_binding = root / "published"
            output_binding.mkdir()

            result = run_job(source, output_binding, root / "scratch")

            self.assertEqual(result.returncode, 1)
            self.assertIn("no CompressedImage topic found", result.stderr)
            self.assertFalse((output_binding / "processing_manifest.json").exists())

    def test_cancellation_does_not_publish_success_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            source = root / "source.mcap"
            source.write_bytes(MCAP_MAGIC + MCAP_MAGIC)
            output_binding = root / "published"
            output_binding.mkdir()
            scratch = root / "scratch"
            fake_module = root / "fake-module"
            fake_module.mkdir()
            fake_runner = fake_module / "run_processing.py"
            fake_runner.write_bytes(RUNNER.read_bytes())
            (fake_module / "convert_mcap_stereo_h264.py").write_text(
                "import time\n"
                "class ConverterConfig:\n"
                "    pass\n"
                "class StereoSplitH264Converter:\n"
                "    def __init__(self, config):\n"
                "        pass\n"
                "    def convert(self, input_path, output_path):\n"
                "        time.sleep(30)\n",
                encoding="utf-8",
            )
            environment = os.environ.copy()
            environment["PYTHONPATH"] = os.pathsep.join(
                part
                for part in (str(fake_module), environment.get("PYTHONPATH", ""))
                if part
            )
            process = subprocess.Popen(
                job_command(source, output_binding, scratch, runner=fake_runner),
                cwd=JOB_ROOT,
                env=environment,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                text=True,
            )
            try:
                deadline = time.monotonic() + 5
                local_input = scratch / "input" / "source.mcap"
                while not local_input.exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertTrue(local_input.exists(), "processor did not reach scratch staging")
                process.terminate()
                process.wait(timeout=5)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=5)

            self.assertNotEqual(process.returncode, 0)
            self.assertFalse((output_binding / "processing_manifest.json").exists())


if __name__ == "__main__":
    unittest.main()
