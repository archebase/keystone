#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Unified MCAP -> MP4 transcoding pipeline.

Auto-dispatches each MCAP to one of two codecs based on its topics:
  * egostereo : /decxin/rgb/compressed  -> rotate 180, crop one eye, encode H.264
  * egolite   : /egocapture/.../hevc_chunk -> extract HEVC stream, transcode to H.264

Both codecs share the same ffmpeg output profile (libx264 / yuv420p / high@4.1 /
+faststart) so every MP4 is browser-friendly and uniform.

Usage:
  python mcap_to_mp4.py <input.mcap | input_dir> [--output DIR] [--workers N]
                        [--eye left|right] [--fps N] [--force]
"""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

import cv2
import numpy as np
from concurrent.futures import ProcessPoolExecutor, as_completed

# mcap (egolite) and rosbags (egostereo) readers
from mcap.reader import make_reader
from rosbags.highlevel import AnyReader
from rosbags.typesys import Stores, get_typestore


# --- feature topics used to classify an MCAP -------------------------------
DEFAULT_DECXIN_TOPIC = "/decxin/rgb/compressed"
DEFAULT_EGOLITE_TOPIC = "/egocapture/camera/wide/video_frame/hevc_chunk"

# egostereo frame geometry (full frame = left 1920 + right 1920 + 160 metadata)
EYE_WIDTH = 1920
EYE_HEIGHT = 1200
METADATA_WIDTH = 160

# Shared ffmpeg output options (encoder-agnostic), browser-friendly profile.
# NOTE: -level is intentionally NOT here. VideoToolbox fails to initialize when
# -level is set together with raw input (kVTPixelTransferNotSupportedErr / -12902);
# it picks a sane level automatically. Other encoders tolerate the missing -level.
SHARED_OUTPUT_OPTIONS = [
    "-pix_fmt", "yuv420p",
    "-profile:v", "high",
    "-c:a", "aac",        # sources have no audio; kept to match the reference command
    "-movflags", "+faststart",
]

# Encoders that accept an explicit -level (kept for libx264 to match the
# reference command's high@4.1). VideoToolbox is excluded due to the issue above.
LEVEL_CAPABLE_ENCODERS = {"libx264", "h264_nvenc", "h264_qsv", "h264_amf"}
LEVEL_VALUE = "4.1"

# Hardware/software H.264 encoder candidates in priority order.
# Each entry: (encoder_name, quality_args). Quality-first tuning; see plan.
ENCODER_CANDIDATES: list[tuple[str, list[str]]] = [
    ("h264_nvenc",         ["-preset", "p7", "-rc", "vbr", "-cq", "26"]),     # NVIDIA
    ("h264_qsv",           ["-global_quality", "26"]),                         # Intel QuickSync
    ("h264_amf",           ["-rc", "cqp", "-qp_i", "22", "-qp_p", "22"]),     # AMD
    ("h264_videotoolbox",  ["-q:v", "65"]),                                    # Apple Silicon
    ("libx264",            ["-preset", "medium", "-crf", "20"]),              # CPU fallback
]
FALLBACK_ENCODER = "libx264"


def build_output_args(encoder: str) -> list[str]:
    """Full ffmpeg output args for the given encoder: -c:v <enc> + quality + shared."""
    quality = next((q for name, q in ENCODER_CANDIDATES if name == encoder), None)
    if quality is None:
        raise ValueError(f"unknown encoder: {encoder}")
    args = ["-c:v", encoder, *quality, *SHARED_OUTPUT_OPTIONS]
    if encoder in LEVEL_CAPABLE_ENCODERS:
        args += ["-level", LEVEL_VALUE]
    return args


# Encoders that require a specific input pixel format fed via a format filter.
# VideoToolbox fails to initialize on bgr24 raw input (kVTPixelTransferNotSupportedErr);
# converting to nv12 upstream fixes it. Other encoders accept the input as-is.
ENCODER_INPUT_FORMAT: dict[str, str] = {
    "h264_videotoolbox": "nv12",
}


def input_filter_args(encoder: str) -> list[str]:
    """Return -vf format=<fmt> if the encoder needs a specific input pixel format."""
    fmt = ENCODER_INPUT_FORMAT.get(encoder)
    if not fmt:
        return []
    return ["-vf", f"format={fmt}"]


def list_available_encoders() -> set[str]:
    """Cheap first-pass filter: which candidate names ffmpeg knows at all."""
    r = subprocess.run(
        ["ffmpeg", "-hide_banner", "-encoders"], capture_output=True, text=True
    )
    available: set[str] = set()
    for name, _ in ENCODER_CANDIDATES:
        if name in r.stdout:
            available.add(name)
    return available


def probe_encoder(name: str) -> bool:
    """Actually encode 1 test frame with this encoder; True if it succeeds.

    Listing an encoder in `ffmpeg -encoders` does not guarantee it is usable
    (missing drivers, disabled iGPU, occupied GPU). A real 1-frame probe is the
    reliable test.
    """
    quality = next((q for n, q in ENCODER_CANDIDATES if n == name), [])
    cmd = [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
        "-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-t", "1",
        "-c:v", name, *quality, "-pix_fmt", "yuv420p",
        "-f", "mp4", os.devnull,
    ]
    try:
        return subprocess.run(cmd, capture_output=True, timeout=30).returncode == 0
    except subprocess.TimeoutExpired:
        return False


def select_encoder(preferred: str | None) -> str:
    """Pick an encoder. If preferred is given, probe only it (fail loudly if unusable).

    Otherwise probe candidates in priority order and return the first usable one.
    Prints the probing progress to stderr.
    """
    known = list_available_encoders()

    def _probe_and_report(name: str) -> bool:
        if name not in known:
            print(f"[enc] {name}: not built into ffmpeg", file=sys.stderr)
            return False
        ok = probe_encoder(name)
        print(f"[enc] {name}: {'ok' if ok else 'probe FAILED'}", file=sys.stderr)
        return ok

    if preferred:
        if preferred not in {n for n, _ in ENCODER_CANDIDATES}:
            raise SystemExit(f"unknown encoder name: {preferred}")
        if not _probe_and_report(preferred):
            raise SystemExit(f"requested encoder {preferred} is not usable on this machine")
        print(f"[enc] selected: {preferred} (manual)", file=sys.stderr)
        return preferred

    for name, _ in ENCODER_CANDIDATES:
        if _probe_and_report(name):
            print(f"[enc] selected: {name} (auto)", file=sys.stderr)
            return name
    # Should be unreachable (libx264 is always available), but be explicit.
    raise SystemExit("no usable encoder found")


@dataclass
class PipelineConfig:
    output: Path | None
    workers: int
    eye: str               # "left" | "right"
    fps: float             # 0 => infer from timestamps
    force: bool
    decxin_topic: str
    egolite_topic: str


# ===========================================================================
# Classification
# ===========================================================================

def classify_mcap(mcap_path: Path, decxin_topic: str, egolite_topic: str) -> str | None:
    """Return 'egostereo', 'egolite', or None (unknown/mixed)."""
    topics = _read_topics(mcap_path)
    has_decxin = decxin_topic in topics
    has_egolite = egolite_topic in topics
    if has_decxin and not has_egolite:
        return "egostereo"
    if has_egolite and not has_decxin:
        return "egolite"
    return None  # neither, or both -> skip


def _read_topics(mcap_path: Path) -> set[str]:
    """Best-effort topic listing from summary; falls back to a peek scan."""
    topics: set[str] = set()
    with mcap_path.open("rb") as f:
        reader = make_reader(f)
        summary = reader.get_summary()
        if summary and summary.channels:
            for ch in summary.channels.values():
                topics.add(ch.topic)
            return topics
        # No summary: stream until we've seen all channels.
        seen_channels: set[int] = set()
        for schema, channel, _msg in reader.iter_messages():
            if channel.id in seen_channels:
                continue
            seen_channels.add(channel.id)
            topics.add(channel.topic)
            # Heuristic: stop after a reasonable number of messages with no new channel.
            if len(seen_channels) >= 32:
                break
    return topics


# ===========================================================================
# Shared helpers
# ===========================================================================

def infer_fps(timestamps_ns: list[int]) -> float:
    """fps = (N-1)*1e9 / (t[-1]-t[0]). Returns 0.0 if it cannot be inferred."""
    if len(timestamps_ns) < 2:
        return 0.0
    span_ns = timestamps_ns[-1] - timestamps_ns[0]
    if span_ns <= 0:
        return 0.0
    return (len(timestamps_ns) - 1) * 1e9 / span_ns


def ffmpeg_available() -> bool:
    return shutil.which("ffmpeg") is not None


def base_output_name(mcap_path: Path) -> str:
    """Output filename is the MCAP stem + .mp4 (decoupled from parent dir name)."""
    return f"{mcap_path.stem}.mp4"


def resolve_output_path(mcap_path: Path, codec: str, output_dir: Path | None) -> Path:
    if output_dir is None:
        # In-place: each MCAP sits in its own dir, so the stem is unique enough.
        return mcap_path.parent / base_output_name(mcap_path)
    # Consolidated output dir: many MCAPs share the same stem (e.g. capture.mcap
    # across capture sessions). Use the parent dir name (the unique session id)
    # to avoid collisions. Falls back to stem if parent is the input root.
    parent_name = mcap_path.parent.name
    name = f"{parent_name}.mp4" if parent_name else base_output_name(mcap_path)
    return output_dir / codec / name


# ===========================================================================
# Codec: egostereo (decxin full-frame JPEG -> one eye -> H.264)
# ===========================================================================

class EgostereoCodec:
    """Decode /decxin/rgb/compressed, rotate 180, crop one eye, encode H.264."""

    def __init__(self, topic: str, eye: str, encoder: str) -> None:
        if eye not in ("left", "right"):
            raise ValueError(f"eye must be 'left' or 'right', got {eye!r}")
        self.topic = topic
        self.eye = eye
        self.encoder = encoder
        self.typestore = get_typestore(Stores.ROS2_JAZZY)

    def convert(self, input_path: Path, output_path: Path, fps_override: float) -> int:
        # Decide fps: explicit override, else infer from message timestamps.
        if fps_override > 0:
            fps = fps_override
        else:
            timestamps = self._collect_timestamps(input_path)
            fps = infer_fps(timestamps)
            if fps <= 0:
                raise RuntimeError("could not infer fps; pass --fps explicitly")
            print(f"  inferred fps={fps:.3f}", file=sys.stderr)

        proc = self._open_ffmpeg(output_path, fps)
        written = 0
        try:
            with AnyReader([input_path], default_typestore=self.typestore) as reader:
                connections = [
                    c for c in reader.connections
                    if c.topic == self.topic and c.msgtype == "sensor_msgs/msg/CompressedImage"
                ]
                if not connections:
                    raise RuntimeError(f"topic not found: {self.topic}")

                for _conn, _ts, rawdata in reader.messages(connections):
                    compressed = self.typestore.deserialize_cdr(rawdata, "sensor_msgs/msg/CompressedImage")
                    bgr = cv2.imdecode(np.asarray(compressed.data, dtype=np.uint8), cv2.IMREAD_COLOR)
                    if bgr is None or not bgr.size:
                        continue
                    frame = cv2.rotate(bgr, cv2.ROTATE_180)
                    eye = self._extract_eye(frame)
                    proc.stdin.write(eye.tobytes())
                    written += 1
        finally:
            assert proc.stdin is not None
            proc.stdin.close()
            rc = proc.wait()
            if rc != 0:
                raise RuntimeError(f"ffmpeg exited with code {rc}")
        return written

    def _collect_timestamps(self, input_path: Path) -> list[int]:
        ts: list[int] = []
        with AnyReader([input_path], default_typestore=self.typestore) as reader:
            connections = [
                c for c in reader.connections
                if c.topic == self.topic and c.msgtype == "sensor_msgs/msg/CompressedImage"
            ]
            for _conn, timestamp_ns, _raw in reader.messages(connections):
                ts.append(int(timestamp_ns))
        return ts

    def _extract_eye(self, frame: np.ndarray) -> np.ndarray:
        required_width = EYE_WIDTH * 2
        if frame.shape[1] < required_width + METADATA_WIDTH or frame.shape[0] < EYE_HEIGHT:
            raise RuntimeError(
                f"frame {frame.shape[1]}x{frame.shape[0]} smaller than expected "
                f"{required_width + METADATA_WIDTH}x{EYE_HEIGHT}"
            )
        # After 180-degree rotation: cols 0:1920 = right eye, 1920:3840 = left eye,
        # 3840:4000 = metadata strip (discarded).
        if self.eye == "left":
            eye = frame[0:EYE_HEIGHT, EYE_WIDTH:EYE_WIDTH * 2]
        else:
            eye = frame[0:EYE_HEIGHT, 0:EYE_WIDTH]
        return np.ascontiguousarray(eye)

    def _open_ffmpeg(self, output_path: Path, fps: float) -> subprocess.Popen:
        output_path.parent.mkdir(parents=True, exist_ok=True)
        cmd = [
            "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
            "-f", "rawvideo", "-pix_fmt", "bgr24",
            "-s", f"{EYE_WIDTH}x{EYE_HEIGHT}",
            "-r", f"{fps}",
            "-i", "pipe:0",
            *input_filter_args(self.encoder),
            *build_output_args(self.encoder),
            str(output_path),
        ]
        return subprocess.Popen(cmd, stdin=subprocess.PIPE)


# ===========================================================================
# Codec: egolite (egocapture HEVC chunks -> transcode to H.264)
# ===========================================================================

def _read_varint(data: bytes, offset: int) -> tuple[int, int]:
    value = 0
    shift = 0
    while offset < len(data):
        byte = data[offset]
        offset += 1
        value |= (byte & 0x7F) << shift
        if byte < 0x80:
            return value, offset
        shift += 7
        if shift >= 64:
            raise ValueError("protobuf varint is too long")
    raise ValueError("truncated protobuf varint")


def _compressed_video_payload(message: bytes) -> bytes | None:
    """Return field 3 (data) from foxglove.CompressedVideo without protobuf deps."""
    offset = 0
    while offset < len(message):
        key, offset = _read_varint(message, offset)
        field_number = key >> 3
        wire_type = key & 0x07
        if wire_type == 0:
            _, offset = _read_varint(message, offset)
        elif wire_type == 1:
            offset += 8
        elif wire_type == 2:
            length, offset = _read_varint(message, offset)
            end = offset + length
            if end > len(message):
                raise ValueError("truncated protobuf length-delimited field")
            payload = message[offset:end]
            offset = end
            if field_number == 3:
                return payload
        elif wire_type == 5:
            offset += 4
        else:
            raise ValueError(f"unsupported protobuf wire type: {wire_type}")
    return None


class EgoliteCodec:
    """Extract HEVC chunks from egocapture MCAP and transcode to H.264."""

    def __init__(self, topic: str, encoder: str) -> None:
        self.topic = topic
        self.encoder = encoder

    def convert(self, input_path: Path, output_path: Path, fps_override: float) -> int:
        output_path.parent.mkdir(parents=True, exist_ok=True)

        # Decide fps: explicit override, else infer from publish timestamps,
        # else fall back to 30.
        if fps_override > 0:
            fps = fps_override
        else:
            fps = self._infer_fps(input_path)
            if fps <= 0:
                fps = 30.0
                print(f"  could not infer fps, falling back to {fps}", file=sys.stderr)
            else:
                print(f"  inferred fps={fps:.3f}", file=sys.stderr)

        with tempfile.TemporaryDirectory(prefix="egolite_") as tmp:
            hevc_path = Path(tmp) / f"{input_path.stem}.hevc"
            frame_count = self._extract_hevc_stream(input_path, hevc_path)
            if frame_count == 0:
                raise RuntimeError(f"no HEVC frames extracted from {input_path}")
            self._transcode(hevc_path, output_path, fps)
        return frame_count

    def _extract_hevc_stream(self, input_path: Path, hevc_path: Path) -> int:
        frame_count = 0
        with input_path.open("rb") as src, hevc_path.open("wb") as sink:
            reader = make_reader(src)
            for _schema, _channel, msg in reader.iter_messages(topics=[self.topic]):
                payload = _compressed_video_payload(msg.data)
                if not payload:
                    continue
                sink.write(payload)
                frame_count += 1
        return frame_count

    def _infer_fps(self, input_path: Path) -> float:
        ts: list[int] = []
        with input_path.open("rb") as f:
            reader = make_reader(f)
            for _schema, _channel, msg in reader.iter_messages(topics=[self.topic]):
                ts.append(msg.log_time if hasattr(msg, "log_time") else msg.publish_time)
        return infer_fps(ts)

    def _transcode(self, hevc_path: Path, output_path: Path, fps: float) -> None:
        cmd = [
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
            "-r", f"{fps}",
            "-i", str(hevc_path),
            *input_filter_args(self.encoder),
            *build_output_args(self.encoder),
            str(output_path),
        ]
        subprocess.run(cmd, check=True)


# ===========================================================================
# Per-file task (runs in worker process)
# ===========================================================================

@dataclass
class TaskResult:
    mcap_path: str
    codec: str          # egostereo | egolite | skip | error
    output: str
    frames: int
    error: str
    encoder: str = ""        # encoder actually used
    fell_back: bool = False  # True if hwenc failed and we retried with libx264


def _convert_with_codec(codec: str, encoder: str, mcap_path: Path,
                        out_path: Path, eye: str, fps: float,
                        decxin_topic: str, egolite_topic: str) -> int:
    """Run one codec with a specific encoder; raises on failure."""
    if codec == "egostereo":
        return EgostereoCodec(decxin_topic, eye, encoder).convert(mcap_path, out_path, fps)
    return EgoliteCodec(egolite_topic, encoder).convert(mcap_path, out_path, fps)


def _process_one(args: tuple) -> TaskResult:
    (mcap_str, output_str, eye, fps, force, encoder,
     decxin_topic, egolite_topic) = args
    mcap_path = Path(mcap_str)
    output_dir = Path(output_str) if output_str else None

    try:
        codec = classify_mcap(mcap_path, decxin_topic, egolite_topic)
        if codec is None:
            return TaskResult(str(mcap_path), "skip", "", 0,
                              "no/mixed feature topic")

        out_path = resolve_output_path(mcap_path, codec, output_dir)
        if out_path.exists() and not force:
            return TaskResult(str(mcap_path), codec, str(out_path), 0,
                              "skip existing")

        # Try the selected encoder first; on failure, fall back to libx264
        # (runtime fallback) and retry the whole file once.
        try:
            frames = _convert_with_codec(codec, encoder, mcap_path, out_path,
                                         eye, fps, decxin_topic, egolite_topic)
            return TaskResult(str(mcap_path), codec, str(out_path), frames,
                              "", encoder, False)
        except Exception as exc:  # noqa: BLE001
            if encoder == FALLBACK_ENCODER:
                raise  # already on CPU, nothing to fall back to
            # Clean any partial output before retrying.
            out_path.unlink(missing_ok=True)
            print(f"  [hwfail] {encoder} failed ({exc}); retrying with {FALLBACK_ENCODER}",
                  file=sys.stderr)
            frames = _convert_with_codec(codec, FALLBACK_ENCODER, mcap_path, out_path,
                                         eye, fps, decxin_topic, egolite_topic)
            return TaskResult(str(mcap_path), codec, str(out_path), frames,
                              "", FALLBACK_ENCODER, True)
    except Exception as exc:  # noqa: BLE001 - isolate failure per file
        return TaskResult(str(mcap_path), "error", "", 0, f"{type(exc).__name__}: {exc}")


# ===========================================================================
# Input scanning
# ===========================================================================

def discover_mcaps(input_path: Path) -> list[Path]:
    if input_path.is_file():
        if input_path.suffix.lower() != ".mcap":
            raise SystemExit(f"input file is not an .mcap: {input_path}")
        return [input_path]
    if input_path.is_dir():
        return sorted(input_path.rglob("*.mcap"))
    raise SystemExit(f"input not found: {input_path}")


# ===========================================================================
# CLI
# ===========================================================================

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Unified MCAP -> MP4 pipeline. Auto-dispatches decxin (egostereo) "
                    "and egocapture/capture (egolite) recordings to H.264 MP4."
    )
    p.add_argument("input", type=Path,
                   help="An .mcap file, or a directory to scan recursively for *.mcap.")
    p.add_argument("--output", type=Path, default=None,
                   help="Output directory. If set: <output>/<codec>/<name>.mp4; "
                        "otherwise next to each source MCAP.")
    p.add_argument("--workers", type=int, default=max(1, (os.cpu_count() or 2) // 2),
                   help="Number of parallel worker processes (default: half of CPUs).")
    p.add_argument("--eye", choices=["left", "right"], default="left",
                   help="Which eye egostereo crops (default: left).")
    p.add_argument("--fps", type=float, default=0.0,
                   help="Force output fps. 0 = infer from message timestamps.")
    p.add_argument("--force", action="store_true",
                   help="Overwrite existing output files (default: skip).")
    p.add_argument("--encoder", default=None,
                   help="Force a specific H.264 encoder (e.g. libx264, h264_nvenc, "
                        "h264_qsv, h264_amf, h264_videotoolbox). Default: auto-detect "
                        "best available; on runtime failure fall back to libx264.")
    p.add_argument("--decxin-topic", default=DEFAULT_DECXIN_TOPIC)
    p.add_argument("--egolite-topic", default=DEFAULT_EGOLITE_TOPIC)
    return p


def main(argv: Iterable[str] | None = None) -> int:
    args = build_parser().parse_args(argv)

    if not ffmpeg_available():
        print("ffmpeg is required but was not found on PATH", file=sys.stderr)
        return 2

    # Probe and select the encoder once in the main process; pass it to workers.
    encoder = select_encoder(args.encoder)

    if args.output is not None:
        args.output.mkdir(parents=True, exist_ok=True)

    mcaps = discover_mcaps(args.input)
    if not mcaps:
        print(f"no .mcap files found under {args.input}", file=sys.stderr)
        return 1

    total = len(mcaps)
    print(f"discovered {total} mcap file(s); workers={args.workers}; encoder={encoder}",
          file=sys.stderr)

    tasks = [
        (
            str(m),
            str(args.output) if args.output else None,
            args.eye, args.fps, args.force, encoder,
            args.decxin_topic, args.egolite_topic,
        )
        for m in mcaps
    ]

    done = 0
    ok = 0
    skipped = 0
    failed = 0
    with ProcessPoolExecutor(max_workers=args.workers) as pool:
        futures = {pool.submit(_process_one, t): t for t in tasks}
        for fut in as_completed(futures):
            res: TaskResult = fut.result()
            done += 1
            tag = res.codec
            fb = " [hwfail->cpu]" if res.fell_back else ""
            if tag in ("egostereo", "egolite"):
                if res.error == "skip existing":
                    skipped += 1
                    print(f"[{done}/{total}] [skip] {res.mcap_path} (exists)", file=sys.stderr)
                else:
                    ok += 1
                    enc_info = f" @{res.encoder}" if res.fell_back else ""
                    print(f"[{done}/{total}] [{tag}{fb}] {res.mcap_path} -> {res.output} "
                          f"({res.frames} frames{enc_info})", file=sys.stderr)
            elif tag == "skip":
                skipped += 1
                print(f"[{done}/{total}] [skip] {res.mcap_path} ({res.error})", file=sys.stderr)
            else:  # error
                failed += 1
                print(f"[{done}/{total}] [ERROR] {res.mcap_path}: {res.error}", file=sys.stderr)

    print(f"done: ok={ok} skipped={skipped} failed={failed} total={total}", file=sys.stderr)
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
