# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import importlib.util
import io
import json
import tarfile
import tempfile
import unittest
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from run_processing import find_root, safe_extract


def has_module(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except ModuleNotFoundError:
        return False


class E2JobContractTest(unittest.TestCase):
    def make_tar(self, root: Path, members: dict[str, bytes]) -> Path:
        archive = root / "capture.tar"
        with tarfile.open(archive, "w") as tar:
            for name, data in members.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
        return archive

    @unittest.skipUnless(
        has_module("numpy")
        and has_module("google.protobuf")
        and has_module("mcap")
        and has_module("rosbags"),
        "full E2 converter dependencies are available in the Job image",
    )
    def test_builds_standard_calibration_with_imu_and_time_extrinsics(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for camera, eye, position in (
                ("Camera0", "left", [0.0, 0.0, 0.0]),
                ("Camera1", "right", [0.1, 0.0, 0.0]),
            ):
                (root / camera).mkdir()
                (root / camera / "camera_params.json").write_text(json.dumps({
                    "group": "tracking",
                    "cameras": [{
                        "eye": eye, "width": 1600, "height": 1200,
                        "intrinsics": {
                            "focalX": 500, "focalY": 501, "centerX": 800, "centerY": 600,
                            "radialDistortion": [1, 2, 3, 4, 5],
                        },
                        "extrinsics": {"position": position, "rotation": [0, 0, 0, 1]},
                    }],
                }))
            (root / "Sensors").mkdir()
            (root / "Sensors" / "imu_calibration.json").write_text(json.dumps({
                "imu": {"time_alignment_s": {"cameras": {
                    "rgb-left": -0.001, "rgb-right": -0.002,
                }}},
                "noise": {
                    "accel_noise_std_mps2": [0.02, 0.02, 0.02],
                    "accel_bias_std_mps2": [0.05, 0.05, 0.05],
                    "gyro_noise_std_rads": [0.0016, 0.0016, 0.0016],
                    "gyro_bias_std_rads": [0.005, 0.005, 0.005],
                },
            }))

            from e2_converter import _build_calibration

            calibration = _build_calibration(root)
            self.assertEqual(calibration["schema"], "archebase.calibration")
            self.assertEqual([camera["topic"] for camera in calibration["cameras"]], [
                "/camera/left/image/h264", "/camera/right/image/h264",
            ])
            self.assertEqual(calibration["cameras"][0]["intrinsics"]["distortion_coefficients"], [1.0, 2.0, 3.0, 4.0])
            self.assertEqual(calibration["imus"][0]["intrinsics"]["accelerometer_noise_density"], 0.02)
            self.assertEqual([(item["from_frame"], item["to_frame"]) for item in calibration["extrinsics"]["transforms"]], [
                ("imu0", "cam0"), ("cam0", "cam1"),
            ])
            self.assertEqual(calibration["extrinsics"]["transforms"][1]["matrix"][0][3], 0.1)
            self.assertEqual([item["offset_seconds"] for item in calibration["temporal_extrinsics"]], [-0.001, -0.002])

    def test_manifest_does_not_advertise_external_calibration(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            outputs = Path(directory)
            for name in ("output_bag.mcap", "metadata.yaml", "calibration.json"):
                (outputs / name).write_bytes(name.encode())

            from run_processing import build_manifest

            manifest = build_manifest(
                {"nominal_fps": 30, "calibration_schema": "archebase.calibration"},
                outputs,
                "tos://bucket/input.tar",
                123,
                1,
                "processor@sha256:digest",
                "2026-01-01T00:00:00Z",
                "2026-01-01T00:00:01Z",
            )

            self.assertNotIn("calibration", manifest)
            self.assertEqual(manifest["outputs"]["calibration"]["name"], "calibration.json")
            self.assertEqual(manifest["calibration_schema"], "archebase.calibration")

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            names = {
                "Camera0/video.mp4": b"left",
                "Camera0/camera_params.json": b"{}",
                "Camera1/video.mp4": b"right",
                "Camera1/camera_params.json": b"{}",
                "Sensors/accel.csv": b"ts_ns,ax,ay,az\n",
                "Sensors/gyro.csv": b"ts_ns,gx,gy,gz\n",
                "Sensors/imu_calibration.json": b"{}",
            }
            archive = self.make_tar(root, names)
            extracted = root / "extracted"
            safe_extract(archive, extracted)
            self.assertEqual(find_root(extracted), extracted)

    def test_accepts_one_wrapper_directory(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            names = {
                "capture/Camera0/video.mp4": b"left",
                "capture/Camera0/camera_params.json": b"{}",
                "capture/Camera1/video.mp4": b"right",
                "capture/Camera1/camera_params.json": b"{}",
                "capture/Sensors/accel.csv": b"data",
                "capture/Sensors/gyro.csv": b"data",
                "capture/Sensors/imu_calibration.json": b"{}",
            }
            archive = self.make_tar(root, names)
            extracted = root / "extracted"
            safe_extract(archive, extracted)
            self.assertEqual(find_root(extracted), extracted / "capture")

    def test_rejects_path_traversal(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = self.make_tar(root, {"../escape": b"bad"})
            with self.assertRaises(RuntimeError):
                safe_extract(archive, root / "extracted")

    def test_rejects_symlink(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / "capture.tar"
            with tarfile.open(archive, "w") as tar:
                info = tarfile.TarInfo("escape")
                info.type = tarfile.SYMTYPE
                info.linkname = "/tmp/outside"
                tar.addfile(info)
            with self.assertRaises(RuntimeError):
                safe_extract(archive, root / "extracted")


if __name__ == "__main__":
    unittest.main()
