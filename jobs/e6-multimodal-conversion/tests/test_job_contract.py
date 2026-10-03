# SPDX-FileCopyrightText: 2026 ArcheBase
#
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
from run_processing import build_manifest, find_root, safe_extract


def has_module(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except ModuleNotFoundError:
        return False


FLAT_CAPTURE = {
    "rgb.mp4": b"video",
    "rgb_metainfo.csv": b"frame_index,mid_exposure_utc_ns\n0,1000\n",
    "tracking.mp4": b"video",
    "tracking_metainfo.csv": b"frame_index,mid_exposure_utc_ns\n0,1000\n",
    "ctrl.mp4": b"video",
    "ctrl_metainfo.csv": b"frame_index,mid_exposure_utc_ns\n0,1000\n",
    "head_pose.csv": b"timestamp_ns,pos_x,pos_y,pos_z,quat_x,quat_y,quat_z,quat_w\n",
    "accel.csv": b"timestamp_ns,x,y,z\n",
    "gyro.csv": b"timestamp_ns,x,y,z\n",
    "camera_params_rgb.json": b"{}",
    "imu_calibration.json": b"{}",
}


class E6JobContractTest(unittest.TestCase):
    def make_tar(self, root: Path, members: dict[str, bytes]) -> Path:
        archive = root / "capture.tar"
        with tarfile.open(archive, "w") as tar:
            for name, data in members.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
        return archive

    def test_accepts_a_flat_capture(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = self.make_tar(root, FLAT_CAPTURE)
            extracted = root / "extracted"
            safe_extract(archive, extracted)
            self.assertEqual(find_root(extracted), extracted)

    def test_accepts_one_wrapper_directory(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            names = {f"capture/{name}": data for name, data in FLAT_CAPTURE.items()}
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

    def test_manifest_matches_the_e6_contract(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            outputs = Path(directory)
            for name in ("output_bag.mcap", "metadata.yaml", "calibration.json"):
                (outputs / name).write_bytes(name.encode())

            manifest = build_manifest(
                {"nominal_fps": 30, "calibration_schema": "archebase.calibration"},
                outputs,
                "device-uploads/132/cap/upload/capture.tar",
                12_781_568,
                "a" * 64,
                1,
                "processor@sha256:digest",
                "2026-01-01T00:00:00Z",
                "2026-01-01T00:00:01Z",
            )

            # Keystone rejects a manifest that advertises an external
            # calibration snapshot: the calibration here is a derived output.
            self.assertNotIn("calibration", manifest)
            self.assertEqual(manifest["kind"], "e6_multimodal_conversion")
            self.assertEqual(manifest["output_format"], "h264_ros2_mcap")
            self.assertEqual(manifest["outputs"]["calibration"]["name"], "calibration.json")
            self.assertEqual(manifest["source"]["sha256"], "a" * 64)
            self.assertEqual(set(manifest["outputs"]), {"mcap", "metadata", "calibration"})

    @unittest.skipUnless(
        has_module("numpy") and has_module("google.protobuf") and has_module("mcap")
        and has_module("rosbags"),
        "full E6 converter dependencies are available in the Job image",
    )
    def test_builds_calibration_from_rgb_params_and_imu_document(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "camera_params_rgb.json").write_text(json.dumps({
                "group": "rgb",
                "cameras": [
                    {
                        "eye": "left", "width": 1920, "height": 1200,
                        "intrinsics": {
                            "focalX": 561.5, "focalY": 561.5, "centerX": 950.6, "centerY": 592.3,
                            "radialDistortion": [1, 2, 3, 4, 5],
                        },
                        "extrinsics": {"position": [0.0, 0.0, 0.0], "rotation": [0, 0, 0, 1]},
                    },
                    {
                        "eye": "right", "width": 1920, "height": 1200,
                        "intrinsics": {
                            "focalX": 558.7, "focalY": 558.7, "centerX": 959.2, "centerY": 597.0,
                            "radialDistortion": [1, 2, 3, 4, 5],
                        },
                        "extrinsics": {"position": [0.1, 0.0, 0.0], "rotation": [0, 0, 0, 1]},
                    },
                ],
            }))
            (root / "imu_calibration.json").write_text(json.dumps({
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

            from e6_converter import _build_calibration

            calibration = _build_calibration(root)
            self.assertEqual(calibration["schema"], "archebase.calibration")
            self.assertEqual([camera["topic"] for camera in calibration["cameras"]], [
                "/archebase/camera/left/image/h264", "/archebase/camera/right/image/h264",
            ])
            self.assertEqual(calibration["cameras"][0]["resolution"], [1920, 1200])
            self.assertEqual(
                calibration["cameras"][0]["intrinsics"]["distortion_coefficients"],
                [1.0, 2.0, 3.0, 4.0],
            )
            # Consumers read the intrinsics in the schema's declaration order;
            # the written JSON must not sort keys.
            self.assertEqual(
                list(calibration["cameras"][0]["intrinsics"].keys()),
                ["camera_model", "parameters", "distortion_model", "distortion_coefficients"],
            )
            self.assertEqual(
                calibration["imus"][0]["intrinsics"]["accelerometer_noise_density"], 0.02
            )
            self.assertEqual(
                [(item["from_frame"], item["to_frame"]) for item in calibration["extrinsics"]["transforms"]],
                [("imu0", "cam0"), ("cam0", "cam1")],
            )
            self.assertEqual(
                [item["offset_seconds"] for item in calibration["temporal_extrinsics"]],
                [-0.001, -0.002],
            )
            self.assertNotIn("device_calibration", calibration)


if __name__ == "__main__":
    unittest.main()
