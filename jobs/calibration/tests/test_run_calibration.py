# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest


JOB_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(JOB_ROOT))
import run_calibration  # noqa: E402
sys.path.remove(str(JOB_ROOT))


MCAP_MAGIC = b"\x89MCAP0\r\n"
SESSION_ID = "7f9af590-75c2-47ad-b6e0-76ebf05c44f7"
CAPTURE_ID = "92cd6f2f-d131-4bf0-9b4a-d96258d09011"
CAMERA_SERIAL = "CAMERA-SN-001"
PROCESSOR_IMAGE = "registry.example/calibration@sha256:" + "a" * 64


class CalibrationJobTest(unittest.TestCase):
    def make_args(self, root: Path, source_bytes: bytes):
        source = root / "capture.mcap"
        source.write_bytes(source_bytes)
        return run_calibration.build_parser().parse_args(
            [
                "--input", str(source),
                "--output", str(root / "binding" / "result.json"),
                "--calibration-session-id", SESSION_ID,
                "--capture-id", CAPTURE_ID,
                "--camera-serial", CAMERA_SERIAL,
                "--expected-source-size", str(len(source_bytes)),
                "--expected-source-checksum", hashlib.sha256(source_bytes).hexdigest(),
                "--source-uri", "tos://bucket/calibration-captures/capture.mcap",
                "--processor-image", PROCESSOR_IMAGE,
                "--scratch", str(root / "scratch"),
            ]
        )

    def test_preprocesses_and_wraps_archebase_calibration_result(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source_bytes = MCAP_MAGIC + b"source-records" + MCAP_MAGIC
            args = self.make_args(root, source_bytes)

            def preprocess(source: Path, output: Path):
                self.assertEqual(source, args.input.resolve())
                output.mkdir(parents=True)
                preprocessed = output / "preprocessed.mcap"
                preprocessed.write_bytes(MCAP_MAGIC + b"split" + MCAP_MAGIC)
                return preprocessed, {
                    "input_messages": 2,
                    "decoded_images": 2,
                    "left_images": 2,
                    "right_images": 2,
                    "imu_messages": 11,
                    "skipped_messages": 0,
                }

            def calibrate(preprocessed: Path, output: Path) -> None:
                self.assertEqual(preprocessed.name, "preprocessed.mcap")
                (output / "precheck").mkdir(parents=True)
                (output / "precheck" / "result.json").write_text(
                    json.dumps({"status": "ok", "frames": 2}), encoding="utf-8"
                )
                (output / "calibration_quality.json").write_text(
                    json.dumps({"camera_calibration": {"status": "qualified"}}),
                    encoding="utf-8",
                )
                (output / "calibration.json").write_text(
                    json.dumps({"schema": "archebase.calibration", "schema_version": "1.0"}),
                    encoding="utf-8",
                )

            result = run_calibration.run(args, preprocess=preprocess, calibrate=calibrate)

            published = json.loads(args.output.read_text(encoding="utf-8"))
            self.assertEqual(published, result)
            self.assertEqual(result["status"], "succeeded")
            self.assertEqual(result["camera_serial"], CAMERA_SERIAL)
            self.assertEqual(result["source"]["sha256"], hashlib.sha256(source_bytes).hexdigest())
            self.assertEqual(result["result"]["preprocessing"]["imu_messages"], 11)
            self.assertEqual(result["result"]["precheck"]["status"], "ok")
            self.assertEqual(result["result"]["quality"]["camera_calibration"]["status"], "qualified")
            self.assertEqual(result["result"]["calibration"]["schema"], "archebase.calibration")
            self.assertEqual(
                json.loads((args.output.parent / "calibration.json").read_text(encoding="utf-8"))["schema"],
                "archebase.calibration",
            )

    def test_publishes_failed_result_for_calibration_rejection(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source_bytes = MCAP_MAGIC + b"source-records" + MCAP_MAGIC
            args = self.make_args(root, source_bytes)

            def preprocess(_source: Path, output: Path):
                output.mkdir(parents=True)
                preprocessed = output / "preprocessed.mcap"
                preprocessed.write_bytes(MCAP_MAGIC + b"split" + MCAP_MAGIC)
                return preprocessed, {"decoded_images": 2, "imu_messages": 11}

            def calibrate(_preprocessed: Path, output: Path) -> None:
                output.mkdir(parents=True)
                (output / "error.json").write_text(
                    json.dumps(
                        {
                            "stage": "precheck",
                            "error": {"type": "PipelineError", "message": "target was not visible"},
                        }
                    ),
                    encoding="utf-8",
                )
                raise run_calibration.CalibrationRejected(output / "error.json")

            result = run_calibration.run(args, preprocess=preprocess, calibrate=calibrate)

            self.assertEqual(result["status"], "failed")
            self.assertEqual(result["camera_serial"], CAMERA_SERIAL)
            self.assertIn("target was not visible", result["error_message"])
            self.assertEqual(result["result"]["failure"]["stage"], "precheck")
            self.assertTrue(args.output.is_file())

    def test_publishes_failed_result_for_preprocessing_rejection(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source_bytes = MCAP_MAGIC + b"source-records" + MCAP_MAGIC
            args = self.make_args(root, source_bytes)

            def preprocess(_source: Path, _output: Path):
                raise run_calibration.PreprocessingRejected(
                    "DECXIN preprocessing did not produce IMU samples"
                )

            def calibrate(_preprocessed: Path, _output: Path) -> None:
                self.fail("calibration must not run after preprocessing rejection")

            result = run_calibration.run(args, preprocess=preprocess, calibrate=calibrate)

            published = json.loads(args.output.read_text(encoding="utf-8"))
            self.assertEqual(published, result)
            self.assertEqual(result["status"], "failed")
            self.assertEqual(result["camera_serial"], CAMERA_SERIAL)
            self.assertIn("did not produce IMU samples", result["error_message"])
            self.assertEqual(result["result"]["failure"]["stage"], "preprocessing")
            self.assertEqual(
                result["result"]["failure"]["error"]["type"],
                "PreprocessingRejected",
            )

    def test_source_identity_mismatch_does_not_publish_result(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source_bytes = MCAP_MAGIC + b"source-records" + MCAP_MAGIC
            args = self.make_args(root, source_bytes)
            args.expected_source_checksum = "0" * 64

            with self.assertRaisesRegex(RuntimeError, "source checksum mismatch"):
                run_calibration.run(args)

            self.assertFalse(args.output.exists())


if __name__ == "__main__":
    unittest.main()
