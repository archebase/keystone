# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


JOB_ROOT = Path(__file__).resolve().parents[1]
RUNNER = JOB_ROOT / "run_calibration.py"
MCAP_MAGIC = b"\x89MCAP0\r\n"


class PlaceholderCalibrationJobTest(unittest.TestCase):
    def test_writes_one_result_json_for_one_mcap(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = root / "capture.mcap"
            source_bytes = MCAP_MAGIC + b"placeholder-mcap-records" + MCAP_MAGIC
            source.write_bytes(source_bytes)
            output = root / "output" / "result.json"
            checksum = hashlib.sha256(source_bytes).hexdigest()

            completed = subprocess.run(
                [
                    sys.executable,
                    str(RUNNER),
                    "--input",
                    str(source),
                    "--output",
                    str(output),
                    "--calibration-session-id",
                    "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
                    "--capture-id",
                    "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
                    "--expected-source-size",
                    str(len(source_bytes)),
                    "--expected-source-checksum",
                    checksum,
                    "--source-uri",
                    "tos://bucket/calibration-captures/capture.mcap",
                    "--processor-image",
                    "registry.example/calibration@sha256:" + "a" * 64,
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            result = json.loads(output.read_text(encoding="utf-8"))
            self.assertEqual(result["status"], "succeeded")
            self.assertEqual(result["algorithm_version"], "placeholder-v1")
            self.assertTrue(result["placeholder"])
            self.assertEqual(result["capture_id"], "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
            self.assertEqual(result["source"]["sha256"], checksum)
            self.assertEqual(result["source"]["size_bytes"], len(source_bytes))
            self.assertEqual(result["result"]["camera_matrix"], [[1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [0.0, 0.0, 1.0]])

    def test_writes_failed_result_for_invalid_mcap(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            source = root / "capture.mcap"
            source_bytes = b"not-an-mcap"
            source.write_bytes(source_bytes)
            output = root / "output" / "result.json"
            checksum = hashlib.sha256(source_bytes).hexdigest()

            completed = subprocess.run(
                [
                    sys.executable,
                    str(RUNNER),
                    "--input",
                    str(source),
                    "--output",
                    str(output),
                    "--calibration-session-id",
                    "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
                    "--capture-id",
                    "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
                    "--expected-source-size",
                    str(len(source_bytes)),
                    "--expected-source-checksum",
                    checksum,
                    "--source-uri",
                    "tos://bucket/calibration-captures/capture.mcap",
                    "--processor-image",
                    "registry.example/calibration@sha256:" + "a" * 64,
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            result = json.loads(output.read_text(encoding="utf-8"))
            self.assertEqual(result["status"], "failed")
            self.assertIn("too small", result["error_message"])
            self.assertEqual(result["source"]["sha256"], checksum)


if __name__ == "__main__":
    unittest.main()
