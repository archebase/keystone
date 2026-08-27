# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import io
import tarfile
import tempfile
import unittest
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from run_processing import find_root, safe_extract


class E2JobContractTest(unittest.TestCase):
    def make_tar(self, root: Path, members: dict[str, bytes]) -> Path:
        archive = root / "capture.tar"
        with tarfile.open(archive, "w") as tar:
            for name, data in members.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
        return archive

    def test_accepts_root_layout(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            names = {
                "Camera0/video.mp4": b"left",
                "Camera0/camera_params.json": b"{}",
                "Camera1/video.mp4": b"right",
                "Camera1/camera_params.json": b"{}",
                "Sensors/accel.csv": b"ts_ns,ax,ay,az\n",
                "Sensors/gyro.csv": b"ts_ns,gx,gy,gz\n",
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
