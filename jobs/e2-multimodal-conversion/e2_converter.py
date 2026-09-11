#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Convert an Ego Portal E2 capture directory into H.264 MCAP outputs."""

from __future__ import annotations

import csv
from dataclasses import dataclass, field
import datetime
from fractions import Fraction
import json
from itertools import chain, islice
from pathlib import Path
import re
import subprocess
import threading
from typing import Iterator, Sequence

import numpy as np

from google.protobuf import descriptor_pb2, descriptor_pool, message_factory, timestamp_pb2
from mcap.reader import make_reader
from mcap.writer import Writer
from rosbags.typesys import Stores, get_typestore


LEFT_TOPIC = "/camera/left/image/h264"
RIGHT_TOPIC = "/camera/right/image/h264"
IMU_TOPIC = "/imu/data"
FOXGLOVE_SCHEMA = "foxglove.CompressedVideo"


def foxglove_compressed_video() -> tuple[bytes, type]:
    file_descriptor = descriptor_pb2.FileDescriptorProto(
        name="foxglove/CompressedVideo.proto", package="foxglove", syntax="proto3",
        dependency=["google/protobuf/timestamp.proto"],
    )
    message = file_descriptor.message_type.add(name="CompressedVideo")
    fields = (
        ("timestamp", 1, descriptor_pb2.FieldDescriptorProto.TYPE_MESSAGE, ".google.protobuf.Timestamp"),
        ("frame_id", 2, descriptor_pb2.FieldDescriptorProto.TYPE_STRING, ""),
        ("data", 3, descriptor_pb2.FieldDescriptorProto.TYPE_BYTES, ""),
        ("format", 4, descriptor_pb2.FieldDescriptorProto.TYPE_STRING, ""),
    )
    for name, number, field_type, type_name in fields:
        field = message.field.add(name=name, number=number, type=field_type,
                                  label=descriptor_pb2.FieldDescriptorProto.LABEL_OPTIONAL)
        if type_name:
            field.type_name = type_name
    pool = descriptor_pool.DescriptorPool()
    pool.AddSerializedFile(timestamp_pb2.DESCRIPTOR.serialized_pb)
    pool.Add(file_descriptor)
    cls = message_factory.GetMessageClass(pool.FindMessageTypeByName(FOXGLOVE_SCHEMA))
    descriptor_set = descriptor_pb2.FileDescriptorSet()
    descriptor_set.file.add().ParseFromString(timestamp_pb2.DESCRIPTOR.serialized_pb)
    descriptor_set.file.add().CopyFrom(file_descriptor)
    return descriptor_set.SerializeToString(), cls


FOXGLOVE_DESCRIPTOR, CompressedVideo = foxglove_compressed_video()


@dataclass(frozen=True)
class VideoFrame:
    timestamp_ns: int
    data: bytes
    sequence: int


@dataclass
class ConversionStats:
    # Emitted stereo pairs, incremented together so the two counters stay equal.
    left_video_frames: int = 0
    right_video_frames: int = 0
    imu_messages: int = 0
    # Source frames that had no partner within the pairing tolerance.
    dropped_left_video_frames: int = 0
    dropped_right_video_frames: int = 0
    left_source_video_frames: int = 0
    right_source_video_frames: int = 0
    pair_tolerance_ns: int = 0
    max_pair_offset_ns: int = 0
    left_timestamp_source: str = ""
    right_timestamp_source: str = ""
    # Signed distance from the IMU's first/last sample to the video's, so QA can
    # see whether the IMU brackets the recording (negative start, positive end).
    imu_start_offset_ns: int = 0
    imu_end_offset_ns: int = 0
    # Encoding diagnostics. A channel whose first encoded access unit is not a
    # keyframe cannot be decoded from its first message, and a source bitstream
    # that already lacks reference frames decodes to frozen pictures.
    left_first_frame_keyframe: bool = True
    right_first_frame_keyframe: bool = True
    left_decode_warnings: int = 0
    right_decode_warnings: int = 0
    first_decode_warning: str = ""


@dataclass
class EncodingReport:
    """Diagnostics gathered while encoding one capture's two channels."""

    decode_warnings: dict[str, list[str]] = field(
        default_factory=lambda: {"left": [], "right": []}
    )
    first_frame_keyframe: dict[str, bool] = field(
        default_factory=lambda: {"left": True, "right": True}
    )


def _access_units(stream: Iterator[bytes]) -> Iterator[bytes]:
    """Split an Annex-B stream at AUD NAL units without buffering the stream."""
    buffer = bytearray()
    marker = re.compile(b"(?:\\x00\\x00\\x00\\x01|\\x00\\x00\\x01)\\x09")
    started = False
    for chunk in stream:
        buffer.extend(chunk)
        while True:
            matches = list(marker.finditer(buffer))
            if not matches:
                break
            if not started:
                del buffer[:matches[0].start()]
                started = True
                continue
            if matches[0].start() == 0:
                if len(matches) < 2:
                    break
                boundary = matches[1].start()
            else:
                boundary = matches[0].start()
            yield bytes(buffer[:boundary])
            del buffer[:boundary]
    if started and buffer:
        yield bytes(buffer)


def _ffmpeg_video(
    path: Path, kept: Sequence[int], total_frames: int, warnings: list[str]
) -> Iterator[bytes]:
    """Re-encode the kept frames of one camera to H.264, one access unit each.

    Frame pacing is passed through rather than forced to the container's
    nominal frame rate: an output frame rate would duplicate frames to fill
    capture gaps, breaking the one-to-one correspondence between encoded
    access units and the per-frame exposure timestamps the device recorded.
    """
    command = [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", str(path),
        "-map", "0:v:0",
    ]
    select = _dropped_frame_filter(kept, total_frames)
    if select is not None:
        command += ["-vf", f"select={select}"]
    command += [
        "-c:v", "libx264", "-preset", "medium", "-profile:v", "high",
        "-pix_fmt", "yuv420p", "-fps_mode", "passthrough", "-bf", "0", "-g", "30", "-keyint_min", "30",
        "-sc_threshold", "0", "-b:v", "12M", "-maxrate", "12M", "-bufsize", "24M",
        "-x264-params", "aud=1:repeat-headers=1", "-an", "-f", "h264", "pipe:1",
    ]
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    assert process.stdout is not None
    assert process.stderr is not None

    def drain_stderr() -> None:
        # Keep reading while the encoder runs: a source whose bitstream lacks
        # reference frames emits far more diagnostics than a pipe buffer holds,
        # and leaving them unread would block ffmpeg mid-encode.
        assert process.stderr is not None
        for raw in process.stderr:
            line = raw.decode("utf-8", errors="replace").strip()
            if line and len(warnings) < 200:
                warnings.append(line)

    drainer = threading.Thread(target=drain_stderr, daemon=True)
    drainer.start()
    try:
        yield from iter(lambda: process.stdout.read(256 * 1024), b"")
        if process.wait() != 0:
            detail = "; ".join(warnings[:3]) if warnings else "no diagnostics"
            raise RuntimeError(f"ffmpeg failed for {path.name}: {detail}")
        drainer.join(timeout=5)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        process.stdout.close()
        process.stderr.close()


def _nominal_fps(path: Path) -> Fraction:
    result = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_streams", "-of", "json", str(path)],
        check=True, capture_output=True, text=True,
    )
    streams = json.loads(result.stdout).get("streams", [])
    if not streams:
        raise RuntimeError(f"video has no stream: {path.name}")
    value = streams[0].get("r_frame_rate") or streams[0].get("avg_frame_rate")
    try:
        fps = Fraction(value)
    except (TypeError, ValueError, ZeroDivisionError) as error:
        raise RuntimeError(f"video has invalid frame rate: {path.name}: {value!r}") from error
    if fps <= 0:
        raise RuntimeError(f"video has invalid frame rate: {path.name}: {value!r}")
    return fps


def _common_nominal_fps(left: Path, right: Path) -> Fraction:
    left_fps = _nominal_fps(left)
    right_fps = _nominal_fps(right)
    tolerance = Fraction(1, 1000)
    if abs(left_fps - right_fps) > tolerance:
        raise RuntimeError(
            "left and right videos have different nominal FPS: "
            f"{float(left_fps):.6f} vs {float(right_fps):.6f}"
        )
    return left_fps


def _fps_manifest_value(fps: Fraction) -> int | float:
    return fps.numerator if fps.denominator == 1 else float(fps)


def _container_frame_count(path: Path) -> int:
    """Frames in the container, without decoding the video.

    `nb_frames` is read straight from the mp4 sample table (fragmented files
    included), so checking the metainfo row count against the video does not
    cost a full decode; counting decoded frames is only the fallback for files
    that do not carry the count.
    """
    report = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0",
         "-show_entries", "stream=nb_frames", "-of", "csv=p=0", str(path)],
        check=True, capture_output=True, text=True,
    )
    fields = report.stdout.strip().splitlines()
    if fields and fields[0].isdigit():
        return int(fields[0])
    counted = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
         "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", str(path)],
        check=True, capture_output=True, text=True,
    )
    fields = counted.stdout.strip().splitlines()
    if not fields or not fields[0].isdigit():
        raise RuntimeError(f"cannot count the frames of {path.name}")
    return int(fields[0])


def _recorded_exposure_timestamps(video: Path) -> list[int] | None:
    """Exposure start of every frame, as recorded next to the video."""
    metainfo = video.parent / "metainfo.csv"
    if not metainfo.is_file():
        return None
    with metainfo.open(newline="", encoding="utf-8") as stream:
        reader = csv.DictReader(stream)
        fields = reader.fieldnames or []
        stamp = next(
            (name for name in ("exposure_start_utc_ns", "exposure_start_ns") if name in fields),
            None,
        )
        if stamp is None:
            return None
        recorded = [int(row[stamp]) for row in reader if row.get(stamp)]
    return recorded or None


def _camera_timestamps(video: Path) -> tuple[list[int], str]:
    """Per-frame capture time in nanoseconds, read from the metainfo.

    The recorder writes one metainfo row per encoded sample, so a row count that
    disagrees with the container means the capture is damaged. There is
    deliberately no fallback to the container's own timestamps: those start at
    zero, which would put the video on a relative time base while the IMU
    samples stay on absolute UTC, producing a bag whose streams sit years apart
    without anything failing.
    """
    recorded = _recorded_exposure_timestamps(video)
    if recorded is None:
        raise RuntimeError(
            f"{video.parent.name}/metainfo.csv is missing or carries no exposure timestamps"
        )
    frames = _container_frame_count(video)
    if len(recorded) != frames:
        raise RuntimeError(
            f"{video.parent.name}: metainfo.csv holds {len(recorded)} rows but "
            f"{video.name} holds {frames} frames"
        )
    return recorded, "metainfo"


# ROS stores a Time's seconds in an int32, so the CDR writer cannot represent an
# absolute timestamp past 2038-01-19 and fails with a bare struct.error. Refuse
# anything beyond 2040 with a message that names the real problem.
_TIMESTAMP_CEILING_NS = int(
    datetime.datetime(2040, 1, 1, tzinfo=datetime.timezone.utc).timestamp()
) * 1_000_000_000


def _validate_timestamp_ceiling(**series: list[int]) -> None:
    for name, values in series.items():
        if values and max(values) > _TIMESTAMP_CEILING_NS:
            raise RuntimeError(
                f"{name} timestamp {max(values)} ns is beyond 2040: the device clock is wrong"
            )


def _validate_time_bases(
    video_first_ns: int, video_last_ns: int, imu_first_ns: int, imu_last_ns: int
) -> None:
    """The camera and the IMU must share one time base.

    Both are converted from the same device clock, so a capture whose two ranges
    do not overlap at all is damaged. Merging it would leave the video and the
    IMU years apart in the bag while every counter still looked healthy.
    """
    if imu_last_ns < video_first_ns or imu_first_ns > video_last_ns:
        raise RuntimeError(
            "camera and IMU timestamps do not overlap: "
            f"video {video_first_ns}..{video_last_ns} ns, "
            f"imu {imu_first_ns}..{imu_last_ns} ns"
        )


def _video_frames(
    path: Path, kept: Sequence[int], timestamps: Sequence[int], warnings: list[str]
) -> Iterator[VideoFrame]:
    count = 0
    for sequence, data in enumerate(
        _access_units(_ffmpeg_video(path, kept, len(timestamps), warnings))
    ):
        if sequence >= len(kept):
            raise RuntimeError(
                f"encoded frame count exceeds the selected frame count for {path.name}: "
                f"encoded={sequence + 1} selected={len(kept)}"
            )
        count = sequence + 1
        yield VideoFrame(timestamps[kept[sequence]], data, sequence)
    if count != len(kept):
        raise RuntimeError(
            f"encoded frame count does not match the selected frame count for {path.name}: "
            f"encoded={count} selected={len(kept)}"
        )


# H.264 NAL unit type of an IDR (keyframe) slice, used to tell whether an
# encoded access unit is independently decodable.
_ANNEX_B_START = re.compile(b"\x00\x00\x00\x01|\x00\x00\x01")
_NAL_IDR_SLICE = 5


def _au_is_keyframe(data: bytes) -> bool:
    """True when the access unit carries an IDR slice."""
    for match in _ANNEX_B_START.finditer(data):
        offset = match.end()
        if offset < len(data) and (data[offset] & 0x1F) == _NAL_IDR_SLICE:
            return True
    return False


def _dropped_frame_filter(kept: Sequence[int], total_frames: int) -> str | None:
    """ffmpeg `select` expression that drops every frame outside `kept`.

    Frames are dropped *before* encoding rather than removed from the encoded
    stream: dropping a frame inside a Group of Pictures would leave the
    following frames referencing a picture the decoder never saw, so the
    channel would decode to frozen or garbled pictures until the next IDR.
    Encoding only the kept frames also gives each channel a fresh first
    keyframe, which carries its own SPS/PPS.
    """
    if not kept:
        return None
    keep = set(kept)
    dropped: list[tuple[int, int]] = []
    for index in range(total_frames):
        if index in keep:
            continue
        if dropped and index == dropped[-1][1] + 1:
            dropped[-1] = (dropped[-1][0], index)
        else:
            dropped.append((index, index))
    if not dropped:
        return None
    # Commas inside a filter argument must be escaped: ffmpeg's filtergraph
    # parser otherwise treats them as filter separators.
    condition = "+".join(f"between(n\\,{start}\\,{end})" for start, end in dropped)
    return f"not({condition})"


def _pair_tolerance_ns(fps: Fraction) -> int:
    """Half a nominal frame period, floored at 5 ms."""
    if fps <= 0:
        return 5_000_000
    return max(int(500_000_000 / float(fps)), 5_000_000)


def _plan_video_pairs(
    left_timestamps: Sequence[int], right_timestamps: Sequence[int], tolerance_ns: int
) -> tuple[list[tuple[int, int]], int, int, int]:
    """Match left/right frames by exposure time instead of by index.

    Both cameras timestamp frames from the same clock, but either pipeline can
    lose frames while it spins up, so frame N of one camera is not necessarily
    simultaneous with frame N of the other. Index pairing would offset every
    later pair by that gap; matching on the recorded exposure timestamps
    recovers the real correspondence and only discards frames that have no
    partner within tolerance_ns.

    Returns (pairs, unpaired_left, unpaired_right, max_offset_ns).
    """
    pairs: list[tuple[int, int]] = []
    max_offset_ns = 0
    right = 0
    for left, timestamp in enumerate(left_timestamps):
        if right >= len(right_timestamps):
            break
        # Advance while the following right frame is at least as close in time.
        while (
            right + 1 < len(right_timestamps)
            and abs(right_timestamps[right + 1] - timestamp)
            <= abs(right_timestamps[right] - timestamp)
        ):
            right += 1
        offset_ns = abs(right_timestamps[right] - timestamp)
        if offset_ns <= tolerance_ns:
            pairs.append((left, right))
            max_offset_ns = max(max_offset_ns, offset_ns)
            right += 1
    return pairs, len(left_timestamps) - len(pairs), len(right_timestamps) - len(pairs), max_offset_ns


def _iter_video_pairs(
    left: Path,
    right: Path,
    left_timestamps: Sequence[int],
    right_timestamps: Sequence[int],
    pairs: Sequence[tuple[int, int]],
    report: EncodingReport,
) -> Iterator[tuple[int, VideoFrame, VideoFrame]]:
    """Emit one pair per planned match, verifying the encoding kept the plan."""
    left_kept = [source for source, _ in pairs]
    right_kept = [target for _, target in pairs]
    left_frames = iter(
        _video_frames(left, left_kept, left_timestamps, report.decode_warnings["left"])
    )
    right_frames = iter(
        _video_frames(right, right_kept, right_timestamps, report.decode_warnings["right"])
    )
    for index in range(len(pairs)):
        left_frame = next(left_frames, None)
        right_frame = next(right_frames, None)
        if left_frame is None or right_frame is None:
            raise RuntimeError("video stream ended before the planned stereo pair")
        if index == 0:
            report.first_frame_keyframe["left"] = _au_is_keyframe(left_frame.data)
            report.first_frame_keyframe["right"] = _au_is_keyframe(right_frame.data)
        yield max(left_frame.timestamp_ns, right_frame.timestamp_ns), left_frame, right_frame
    if next(left_frames, None) is not None or next(right_frames, None) is not None:
        raise RuntimeError("selected frame filter did not match the planned stereo pairs")


def _csv_rows(path: Path) -> Iterator[tuple[int, tuple[float, float, float]]]:
    with path.open(newline="", encoding="utf-8") as stream:
        reader = csv.DictReader(stream)
        fields = reader.fieldnames or []
        # Firmware wrote the device clock as `ts_ns` before the UTC alignment
        # change and writes `ts_utc_ns` afterwards; both are nanoseconds. Accept
        # either so captures from both firmware generations convert.
        stamp = next((name for name in ("ts_utc_ns", "ts_ns") if name in fields), None)
        if stamp is None:
            raise RuntimeError(f"{path.name} has no timestamp column: {fields}")
        values = [name for name in fields if name != stamp][:3]
        for row in reader:
            if not row.get(stamp) or any(not row.get(name) for name in values):
                continue
            yield int(row[stamp]), (float(row[values[0]]),
                                    float(row[values[1]]),
                                    float(row[values[2]]))


def _imu_tolerance_ns(*prefixes: Sequence[tuple[int, tuple[float, float, float]]]) -> int:
    """Half the median sample interval, so the nearest sample is unambiguous."""
    intervals = sorted(
        right[0] - left[0]
        for prefix in prefixes
        for left, right in zip(prefix, prefix[1:])
        if right[0] > left[0]
    )
    if not intervals:
        return 1_000_000
    return max(intervals[len(intervals) // 2] // 2, 1_000)


def _head_and_rest(rows: Iterator[object], count: int):
    head = list(islice(rows, count))
    return head, chain(head, rows)


def _imu_rows(root: Path) -> Iterator[tuple[int, tuple[float, ...]]]:
    """Merge accel and gyro by nearest timestamp.

    The two sensors are sampled independently on the same clock, so a capture
    written after the UTC alignment change offsets their timestamps by a few
    hundred microseconds. Requiring exactly equal timestamps (the previous
    behaviour) silently discarded almost every sample; matching the nearest
    sample within half a period keeps them, and the one-sample lookahead below
    keeps memory flat on long captures.
    """
    accel_head, accelerometer = _head_and_rest(iter(_csv_rows(root / "Sensors/accel.csv")), 64)
    gyro_head, gyroscope = _head_and_rest(iter(_csv_rows(root / "Sensors/gyro.csv")), 64)
    tolerance_ns = _imu_tolerance_ns(accel_head, gyro_head)
    acceleration = next(accelerometer, None)
    rotation = next(gyroscope, None)
    rotation_next = next(gyroscope, None)
    while acceleration is not None and rotation is not None:
        while rotation_next is not None and (
            abs(rotation_next[0] - acceleration[0])
            <= abs(rotation[0] - acceleration[0])
        ):
            rotation = rotation_next
            rotation_next = next(gyroscope, None)
        if abs(rotation[0] - acceleration[0]) <= tolerance_ns:
            yield max(acceleration[0], rotation[0]), (*acceleration[1], *rotation[1])
            rotation = rotation_next
            rotation_next = next(gyroscope, None)
        acceleration = next(accelerometer, None)


def _timestamp(ts_ns: int):
    seconds, nanos = divmod(ts_ns, 1_000_000_000)
    return timestamp_pb2.Timestamp(seconds=seconds, nanos=nanos)


def _camera_transform(camera: dict[str, object]) -> np.ndarray:
    extrinsics = camera.get("extrinsics")
    if not isinstance(extrinsics, dict):
        raise RuntimeError("camera calibration is missing extrinsics")
    position = np.asarray(extrinsics.get("position"), dtype=np.float64)
    quaternion = np.asarray(extrinsics.get("rotation"), dtype=np.float64)
    if position.shape != (3,) or quaternion.shape != (4,):
        raise RuntimeError("camera extrinsics must contain a 3D position and XYZW quaternion")
    norm = np.linalg.norm(quaternion)
    if not np.isfinite(norm) or norm <= 0:
        raise RuntimeError("camera extrinsics quaternion is invalid")
    x, y, z, w = quaternion / norm
    rotation = np.array([
        [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
        [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
        [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)],
    ])
    transform = np.eye(4)
    transform[:3, :3] = rotation
    transform[:3, 3] = position
    return transform


def _camera_calibration(camera: dict[str, object], camera_id: str, topic: str,
                        frame_id: str) -> dict[str, object]:
    intrinsics = camera.get("intrinsics")
    if not isinstance(intrinsics, dict):
        raise RuntimeError("camera calibration is missing intrinsics")
    distortion = intrinsics.get("radialDistortion")
    if not isinstance(distortion, list) or len(distortion) < 4:
        raise RuntimeError("camera radialDistortion must contain at least four values")
    return {
        "id": camera_id,
        "name": camera_id,
        "topic": topic,
        "frame_id": frame_id,
        "resolution": [int(camera["width"]), int(camera["height"])],
        "intrinsics": {
            "camera_model": "pinhole",
            "parameters": {
                "fx": float(intrinsics["focalX"]),
                "fy": float(intrinsics["focalY"]),
                "cx": float(intrinsics["centerX"]),
                "cy": float(intrinsics["centerY"]),
            },
            "distortion_model": "equidistant",
            "distortion_coefficients": [float(value) for value in distortion[:4]],
        },
    }


def _scalar_calibration_value(values: object, name: str) -> float:
    if not isinstance(values, list) or not values or not all(np.isfinite(values)):
        raise RuntimeError(f"IMU calibration field {name} must be a finite array")
    if max(values) - min(values) > 1e-5:
        raise RuntimeError(f"IMU calibration field {name} is not axis-uniform")
    return float(values[0])


def _build_calibration(root: Path) -> dict[str, object]:
    left = json.loads((root / "Camera0/camera_params.json").read_text())["cameras"][0]
    right = json.loads((root / "Camera1/camera_params.json").read_text())["cameras"][0]
    imu_document = json.loads((root / "Sensors/imu_calibration.json").read_text())
    imu = imu_document.get("imu")
    noise = imu_document.get("noise")
    if not isinstance(imu, dict) or not isinstance(noise, dict):
        raise RuntimeError("IMU calibration must contain imu and noise objects")
    time_alignment = imu.get("time_alignment_s")
    if not isinstance(time_alignment, dict):
        raise RuntimeError("IMU calibration is missing time_alignment_s")
    camera_offsets = time_alignment.get("cameras")
    if not isinstance(camera_offsets, dict):
        raise RuntimeError("IMU calibration is missing camera time alignment")
    left_offset = camera_offsets.get("rgb-left")
    right_offset = camera_offsets.get("rgb-right")
    if not isinstance(left_offset, (int, float)) or not np.isfinite(left_offset):
        raise RuntimeError("IMU calibration has an invalid rgb-left time offset")
    if not isinstance(right_offset, (int, float)) or not np.isfinite(right_offset):
        raise RuntimeError("IMU calibration has an invalid rgb-right time offset")
    left_transform = _camera_transform(left)
    right_transform = _camera_transform(right)
    camera_to_camera = right_transform @ np.linalg.inv(left_transform)
    return {
        "schema": "archebase.calibration",
        "schema_version": "1.0",
        "cameras": [
            _camera_calibration(left, "cam0", LEFT_TOPIC, "camera_left_optical"),
            _camera_calibration(right, "cam1", RIGHT_TOPIC, "camera_right_optical"),
        ],
        "imus": [{
            "id": "imu0",
            "topic": IMU_TOPIC,
            "model": "calibrated",
            "update_rate_hz": 800.0,
            "intrinsics": {
                "accelerometer_noise_density": _scalar_calibration_value(
                    noise["accel_noise_std_mps2"], "accel_noise_std_mps2"
                ),
                "accelerometer_random_walk": _scalar_calibration_value(
                    noise["accel_bias_std_mps2"], "accel_bias_std_mps2"
                ),
                "gyroscope_noise_density": _scalar_calibration_value(
                    noise["gyro_noise_std_rads"], "gyro_noise_std_rads"
                ),
                "gyroscope_random_walk": _scalar_calibration_value(
                    noise["gyro_bias_std_rads"], "gyro_bias_std_rads"
                ),
            },
        }],
        "extrinsics": {
            "convention": "p_to = R * p_from + t",
            "transforms": [
                {"from_frame": "imu0", "to_frame": "cam0", "matrix": left_transform.tolist()},
                {"from_frame": "cam0", "to_frame": "cam1", "matrix": camera_to_camera.tolist()},
            ],
        },
        "temporal_extrinsics": [
            {
                "from_clock": "cam0", "to_clock": "imu0",
                "offset_seconds": float(left_offset),
                "convention": "t_imu = t_camera + offset_seconds",
            },
            {
                "from_clock": "cam1", "to_clock": "imu0",
                "offset_seconds": float(right_offset),
                "convention": "t_imu = t_camera + offset_seconds",
            },
        ],
    }


def _yaml_metadata(message_count: int, start_ns: int, end_ns: int,
                   topics: list[tuple[str, str, str, int]]) -> str:
    lines = [
        "rosbag2_bagfile_information:", "  version: 5", "  storage_identifier: mcap",
        f"  duration:\n    nanoseconds: {max(0, end_ns - start_ns)}",
        f"  starting_time:\n    nanoseconds_since_epoch: {start_ns}",
        f"  message_count: {message_count}", "  topics_with_message_count:",
    ]
    for topic, message_type, encoding, topic_count in topics:
        lines += ["  - topic_metadata:", f"      name: {topic}", f"      type: {message_type}",
                  f"      serialization_format: {encoding}", "      offered_qos_profiles: ''",
                  f"    message_count: {topic_count}"]
    lines += ["  compression_format: ''", "  compression_mode: ''", "  relative_file_paths:",
              "  - output_bag.mcap", "  files:", "  - path: output_bag.mcap", f"    starting_time:\n      nanoseconds_since_epoch: {start_ns}",
              f"    duration:\n      nanoseconds: {max(0, end_ns - start_ns)}", f"    message_count: {message_count}"]
    return "\n".join(lines) + "\n"


def convert(root: Path, output: Path, source_uri: str = "", source_size: int = 0,
            generation: int = 1, processor_image: str = "") -> dict[str, object]:
    root = root.resolve()
    output.mkdir(parents=True, exist_ok=True)
    left = root / "Camera0/video.mp4"
    right = root / "Camera1/video.mp4"
    required = [left, right, root / "Camera0/camera_params.json", root / "Camera1/camera_params.json",
                root / "Sensors/accel.csv", root / "Sensors/gyro.csv", root / "Sensors/imu_calibration.json"]
    missing = [str(path.relative_to(root)) for path in required if not path.is_file()]
    if missing:
        raise RuntimeError(f"missing E2 input files: {', '.join(missing)}")

    common_fps = _common_nominal_fps(left, right)
    # Read and check every time series before opening the output: a capture whose
    # clock is wrong, or whose camera and IMU cover different time bases, must
    # fail here rather than produce a bag nobody can line up.
    left_timestamps, left_source = _camera_timestamps(left)
    right_timestamps, right_source = _camera_timestamps(right)
    imu_samples = list(_imu_rows(root))
    if not imu_samples:
        raise RuntimeError("the capture holds no IMU samples")
    _validate_timestamp_ceiling(
        camera_left=left_timestamps,
        camera_right=right_timestamps,
        imu=[stamp for stamp, _ in imu_samples],
    )
    video_first_ns = min(left_timestamps[0], right_timestamps[0])
    video_last_ns = max(left_timestamps[-1], right_timestamps[-1])
    _validate_time_bases(video_first_ns, video_last_ns,
                         imu_samples[0][0], imu_samples[-1][0])
    typestore = get_typestore(Stores.ROS2_HUMBLE)
    imu_type = typestore.types["sensor_msgs/msg/Imu"]
    msg = typestore.types
    with (output / "output_bag.mcap").open("wb") as stream:
        writer = Writer(stream)
        writer.start(profile="", library="archebase e2 multimodal conversion")
        video_schema = writer.register_schema(FOXGLOVE_SCHEMA, "protobuf", FOXGLOVE_DESCRIPTOR)
        left_channel = writer.register_channel(LEFT_TOPIC, "protobuf", video_schema)
        right_channel = writer.register_channel(RIGHT_TOPIC, "protobuf", video_schema)
        imu_definition, _ = typestore.generate_msgdef("sensor_msgs/msg/Imu")
        imu_schema = writer.register_schema("sensor_msgs/msg/Imu", "ros2msg", imu_definition.encode())
        imu_channel = writer.register_channel(IMU_TOPIC, "cdr", imu_schema)
        stats = ConversionStats()
        tolerance_ns = _pair_tolerance_ns(common_fps)
        pairs, unpaired_left, unpaired_right, max_offset_ns = _plan_video_pairs(
            left_timestamps, right_timestamps, tolerance_ns
        )
        if not pairs:
            raise RuntimeError(
                "no stereo frame pair within "
                f"{tolerance_ns} ns: left={len(left_timestamps)} right={len(right_timestamps)}"
            )
        stats.left_source_video_frames = len(left_timestamps)
        stats.right_source_video_frames = len(right_timestamps)
        stats.dropped_left_video_frames = unpaired_left
        stats.dropped_right_video_frames = unpaired_right
        stats.pair_tolerance_ns = tolerance_ns
        stats.max_pair_offset_ns = max_offset_ns
        stats.left_timestamp_source = left_source
        stats.right_timestamp_source = right_source
        stats.imu_start_offset_ns = imu_samples[0][0] - video_first_ns
        stats.imu_end_offset_ns = imu_samples[-1][0] - video_last_ns

        report = EncodingReport()
        video_pairs = iter(
            _iter_video_pairs(left, right, left_timestamps, right_timestamps, pairs, report)
        )
        video_pair = next(video_pairs, None)
        imu = iter(imu_samples)
        imu_sample = next(imu, None)
        sequence = 0
        timestamps: list[int] = []
        while video_pair is not None or imu_sample is not None:
            if video_pair is not None and (
                imu_sample is None or video_pair[0] <= imu_sample[0]
            ):
                ts, left_payload, right_payload = video_pair
                for channel, frame_id, data in (
                    (left_channel, "camera_left_optical", left_payload.data),
                    (right_channel, "camera_right_optical", right_payload.data),
                ):
                    video = CompressedVideo()
                    video.timestamp.seconds = ts // 1_000_000_000
                    video.timestamp.nanos = ts % 1_000_000_000
                    video.frame_id = frame_id
                    video.data = data
                    video.format = "h264"
                    writer.add_message(channel, ts, video.SerializeToString(), ts, sequence)
                stats.left_video_frames += 1
                stats.right_video_frames += 1
                video_pair = next(video_pairs, None)
            else:
                if imu_sample is None:
                    raise AssertionError("IMU merge state is inconsistent")
                ts, values = imu_sample
                stamp = _timestamp(ts)
                imu_message = imu_type(
                    header=msg["std_msgs/msg/Header"](
                        stamp=msg["builtin_interfaces/msg/Time"](
                            sec=stamp.seconds, nanosec=stamp.nanos
                        ),
                        frame_id="imu",
                    ),
                    orientation=msg["geometry_msgs/msg/Quaternion"](
                        x=0.0, y=0.0, z=0.0, w=1.0
                    ),
                    orientation_covariance=np.array(
                        [-1.0] + [0.0] * 8, dtype=np.float64
                    ),
                    angular_velocity=msg["geometry_msgs/msg/Vector3"](
                        x=values[3], y=values[4], z=values[5]
                    ),
                    angular_velocity_covariance=np.zeros(9, dtype=np.float64),
                    linear_acceleration=msg["geometry_msgs/msg/Vector3"](
                        x=values[0], y=values[1], z=values[2]
                    ),
                    linear_acceleration_covariance=np.zeros(9, dtype=np.float64),
                )
                writer.add_message(
                    imu_channel,
                    ts,
                    bytes(typestore.serialize_cdr(imu_message, "sensor_msgs/msg/Imu")),
                    ts,
                    sequence,
                )
                stats.imu_messages += 1
                imu_sample = next(imu, None)
            timestamps.append(ts)
            sequence += 1

        # Encoding diagnostics are only complete once both channels finished.
        stats.left_first_frame_keyframe = report.first_frame_keyframe["left"]
        stats.right_first_frame_keyframe = report.first_frame_keyframe["right"]
        stats.left_decode_warnings = len(report.decode_warnings["left"])
        stats.right_decode_warnings = len(report.decode_warnings["right"])
        for channel in ("left", "right"):
            if report.decode_warnings[channel]:
                stats.first_decode_warning = report.decode_warnings[channel][0][:180]
                break
        if stats.imu_messages == 0:
            raise RuntimeError(
                "no IMU sample paired between accel and gyro: "
                "check that both sensor streams use the same clock"
            )
        writer.finish()
    # The MCAP summary is the source of truth for the metadata counts.
    with (output / "output_bag.mcap").open("rb") as stream:
        summary = make_reader(stream).get_summary()
    message_counts = {
        channel.topic: summary.statistics.channel_message_counts.get(channel.id, 0)
        for channel in summary.channels.values()
    }
    total_message_count = sum(message_counts.values())
    calibration = _build_calibration(root)
    (output / "calibration.json").write_text(json.dumps(calibration, indent=2, sort_keys=True) + "\n")
    start_ns, end_ns = (min(timestamps), max(timestamps)) if timestamps else (0, 0)
    topics = [(LEFT_TOPIC, FOXGLOVE_SCHEMA, "protobuf", message_counts[LEFT_TOPIC]),
              (RIGHT_TOPIC, FOXGLOVE_SCHEMA, "protobuf", message_counts[RIGHT_TOPIC]),
              (IMU_TOPIC, "sensor_msgs/msg/Imu", "cdr", message_counts[IMU_TOPIC])]
    (output / "metadata.yaml").write_text(_yaml_metadata(total_message_count, start_ns, end_ns, topics))
    return {"stats": stats.__dict__, "nominal_fps": _fps_manifest_value(common_fps),
            "calibration_schema": calibration["schema"],
            "generation": generation, "processor_image": processor_image,
            "source": {"uri": source_uri, "size_bytes": source_size}, "output_format": "h264_ros2_mcap"}
