#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Normalize ZJ-WA1-D MCAP depth streams to ROS1 compressedDepth.

The converter preserves depth values losslessly and copies every unrelated MCAP
message unchanged. ``--inspect`` classifies an input without writing a file;
``--verify`` validates conversion invariants and emits a JSON result.

Example:
  python3 scripts/normalize_ros2_depth_to_ros1_compresseddepth.py \\
    --inspect input.mcap

  python3 scripts/normalize_ros2_depth_to_ros1_compresseddepth.py \\
    input.mcap output.mcap

Requires: mcap, opencv-python-headless, numpy
"""

from __future__ import annotations

import argparse
import json
import struct
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Tuple

import cv2
import numpy as np
from mcap.reader import make_reader
from mcap.writer import CompressionType, Writer

TOPIC_MAP = {
    "/zj_humanoid/sensor/realsense_head/aligned_depth_to_color/image_raw":
        "/zj_humanoid/sensor/realsense_head/aligned_depth_to_color/image_raw/compressedDepth",
    "/zj_humanoid/sensor/realsense_up/aligned_depth_to_color/image_raw":
        "/zj_humanoid/sensor/realsense_up/aligned_depth_to_color/image_raw/compressedDepth",
}
RAW_TOPICS = tuple(TOPIC_MAP)
TARGET_TOPICS = tuple(TOPIC_MAP.values())
COMPRESSED_FORMAT = b"16UC1; compressedDepth png"
DEPTH_PREFIX = struct.pack("<fff", 0.0, 0.0, 0.0)
ROS1_SCHEMA = b"""# This message contains a compressed image

Header header
string format
uint8[] data
================================================================================
MSG: std_msgs/Header
uint32 seq
time stamp
string frame_id
"""


@dataclass
class Reader:
    pos: int = 0

    def unpack(self, fmt: str, buf: bytes) -> Tuple:
        value = struct.unpack_from(fmt, buf, self.pos)
        self.pos += struct.calcsize(fmt)
        return value

    def string(self, buf: bytes) -> str:
        (size,) = self.unpack("<I", buf)
        value = buf[self.pos:self.pos + size]
        if len(value) != size:
            raise ValueError("truncated string")
        self.pos += size
        return value.decode("utf-8")


def parse_ros2_image(data: bytes) -> Dict[str, object]:
    # The source writer serializes Image byte-packed without CDR alignment.
    r = Reader()
    seq, sec, nsec = r.unpack("<III", data)
    frame_id = r.string(data)
    height, width = r.unpack("<II", data)
    encoding = r.string(data)
    (is_bigendian,) = r.unpack("<B", data)
    (step,) = r.unpack("<I", data)
    (array_len,) = r.unpack("<I", data)
    payload = data[r.pos:r.pos + array_len]
    if len(payload) != array_len:
        raise ValueError("truncated image data")
    return {
        "seq": seq, "sec": sec, "nsec": nsec, "frame_id": frame_id,
        "height": height, "width": width, "encoding": encoding,
        "is_bigendian": is_bigendian, "step": step, "data": payload,
    }


def parse_ros1_compressed_image(data: bytes) -> Dict[str, object]:
    r = Reader()
    seq, sec, nsec = r.unpack("<III", data)
    frame_id = r.string(data)
    image_format = r.string(data)
    (array_len,) = r.unpack("<I", data)
    payload = data[r.pos:r.pos + array_len]
    if len(payload) != array_len:
        raise ValueError("truncated compressed image data")
    return {"seq": seq, "sec": sec, "nsec": nsec, "frame_id": frame_id,
            "format": image_format, "data": payload}


def summary_info(path: Path) -> Dict[str, object]:
    with path.open("rb") as stream:
        reader = make_reader(stream)
        summary = reader.get_summary()
        if summary is None:
            raise RuntimeError(f"{path}: MCAP summary is missing")
        channels = {}
        for channel in (summary.channels or {}).values():
            schema = (summary.schemas or {}).get(channel.schema_id)
            counts = (summary.statistics.channel_message_counts if summary.statistics else {})
            channels[channel.topic] = {
                "count": int(counts.get(channel.id, 0)),
                "message_encoding": channel.message_encoding,
                "schema": schema.name if schema else "",
                "schema_encoding": schema.encoding if schema else "",
            }
        return {
            "total_count": int(summary.statistics.message_count if summary.statistics else sum(x["count"] for x in channels.values())),
            "channels": channels,
        }


def topic_info(info: Dict[str, object], topic: str) -> Dict[str, object]:
    return (info["channels"] or {}).get(topic, {  # type: ignore[union-attr]
        "count": 0, "message_encoding": "", "schema": "", "schema_encoding": "",
    })


def raw_depth_matches(info: Dict[str, object], topic: str) -> bool:
    value = topic_info(info, topic)
    return value["count"] > 0 and value["message_encoding"] == "cdr" and value["schema"] == "sensor_msgs/Image" and value["schema_encoding"] == "ros2msg"  # type: ignore[index]


def compressed_depth_matches(info: Dict[str, object], topic: str) -> bool:
    value = topic_info(info, topic)
    return value["count"] > 0 and value["message_encoding"] == "ros1" and value["schema"] == "sensor_msgs/CompressedImage" and value["schema_encoding"] == "ros1msg"  # type: ignore[index]


def inspect(path: Path) -> Dict[str, object]:
    info = summary_info(path)
    raw = {topic: raw_depth_matches(info, topic) for topic in RAW_TOPICS}
    compressed = {topic: compressed_depth_matches(info, topic) for topic in TARGET_TOPICS}
    raw_channels = [topic for topic in RAW_TOPICS if topic_info(info, topic)["count"] > 0]
    if all(raw.values()) and not any(compressed.values()):
        status = "requires_normalization"
    elif all(compressed.values()) and not any(raw.values()):
        status = "already_compresseddepth"
    else:
        status = "unsupported"
    return {
        "status": status,
        "total_count": info["total_count"],
        "raw_topics": raw,
        "target_topics": compressed,
        "channels": info["channels"],
        "raw_channels_present": raw_channels,
    }


def encode_lossless_depth_png(data: bytes, height: int, width: int) -> bytes:
    depth = np.frombuffer(data, dtype="<u2").reshape(height, width)
    ok, png = cv2.imencode(".png", depth, [int(cv2.IMWRITE_PNG_COMPRESSION), 3])
    if not ok:
        raise RuntimeError("OpenCV failed to encode depth PNG")
    return png.tobytes()


def serialize_ros1_compressed_depth(image: Dict[str, object], png: bytes) -> bytes:
    frame_id = str(image["frame_id"]).encode("utf-8")
    size = 4 + 4 + 4 + 4 + len(frame_id) + 4 + len(COMPRESSED_FORMAT) + 4 + len(png)
    out = bytearray(size)
    pos = 0

    def put(fmt: str, *values: object) -> None:
        nonlocal pos
        struct.pack_into(fmt, out, pos, *values)
        pos += struct.calcsize(fmt)

    def put_bytes(value: bytes) -> None:
        nonlocal pos
        put("<I", len(value))
        out[pos:pos + len(value)] = value
        pos += len(value)

    put("<I", image["seq"])
    put("<iI", image["sec"], image["nsec"])
    put_bytes(frame_id)
    put_bytes(COMPRESSED_FORMAT)
    put_bytes(png)
    return bytes(out)


def verify(source: Path, output: Path) -> Dict[str, object]:
    source_info = inspect(source)
    output_info = inspect(output)
    checks = {
        "source_requires_normalization": source_info["status"] == "requires_normalization",
        "output_compresseddepth": output_info["status"] == "already_compresseddepth",
        "total_count_equal": source_info["total_count"] == output_info["total_count"],
    }
    topic_counts = {}
    for raw_topic, target_topic in TOPIC_MAP.items():
        source_count = topic_info(source_info, raw_topic)["count"]
        target_count = topic_info(output_info, target_topic)["count"]
        checks[f"count_equal:{target_topic}"] = source_count == target_count
        topic_counts[target_topic] = {"source": source_count, "output": target_count}

    # Pixel-check the first converted frame from each depth topic. This catches
    # wiring mistakes without making every production conversion decode all frames.
    for raw_topic, target_topic in TOPIC_MAP.items():
        source_message = first_message(source, raw_topic)
        output_message = first_message(output, target_topic)
        if source_message is None or output_message is None:
            checks[f"lossless_sample:{target_topic}"] = False
            continue
        raw = parse_ros2_image(source_message)
        compressed = parse_ros1_compressed_image(output_message)
        png = compressed["data"][len(DEPTH_PREFIX):]
        decoded = cv2.imdecode(np.frombuffer(png, dtype=np.uint8), cv2.IMREAD_UNCHANGED)
        expected = np.frombuffer(raw["data"], dtype="<u2").reshape(raw["height"], raw["width"])  # type: ignore[attr-defined]
        checks[f"lossless_sample:{target_topic}"] = (
            decoded is not None and decoded.shape == expected.shape and
            decoded.dtype == expected.dtype and bool(np.array_equal(decoded, expected))
        )
    return {
        "source_status": source_info["status"],
        "output_status": output_info["status"],
        "source_total_count": source_info["total_count"],
        "output_total_count": output_info["total_count"],
        "topic_counts": topic_counts,
        "checks": checks,
        "valid": all(checks.values()),
    }


def first_message(path: Path, topic: str) -> bytes | None:
    with path.open("rb") as stream:
        for _, channel, message in make_reader(stream).iter_messages(topics=[topic]):
            return message.data
    return None


def convert(source_path: Path, output_path: Path, limit: int | None, no_compression: bool) -> Dict[str, object]:
    if source_path.resolve() == output_path.resolve():
        raise RuntimeError("input and output must be different files")
    admission = inspect(source_path)
    if admission["status"] != "requires_normalization":
        raise RuntimeError(f"input depth format is {admission['status']}, expected requires_normalization")
    output_path.parent.mkdir(parents=True, exist_ok=True)

    with source_path.open("rb") as source_file:
        reader = make_reader(source_file)
        header = reader.get_header()
        summary = reader.get_summary()
        old_schemas = dict(summary.schemas or {})
        old_channels = dict(summary.channels or {})
        converted_by_topic = {topic: 0 for topic in RAW_TOPICS}

        with output_path.open("wb") as output_file:
            compression = CompressionType.NONE if no_compression else CompressionType.ZSTD
            writer = Writer(output_file, compression=compression)
            writer.start(profile=header.profile, library="normalize-depth/1.0")
            ros1_schema_id = writer.register_schema(
                "sensor_msgs/CompressedImage", "ros1msg", ROS1_SCHEMA
            )
            schema_map: Dict[int, int] = {}
            for schema in old_schemas.values():
                if schema.name == "sensor_msgs/Image" and schema.encoding == "ros2msg":
                    continue
                schema_map[schema.id] = writer.register_schema(schema.name, schema.encoding, schema.data)
            depth_schema_ids = [
                schema.id for schema in old_schemas.values()
                if schema.name == "sensor_msgs/Image" and schema.encoding == "ros2msg"
            ]
            if not depth_schema_ids:
                raise RuntimeError("source ROS2 Image schema not found")
            for schema_id in depth_schema_ids:
                schema_map[schema_id] = ros1_schema_id

            channel_ids: Dict[int, int] = {}
            for channel in old_channels.values():
                if channel.message_encoding == "cdr" and channel.topic in TOPIC_MAP:
                    target_id = channel_ids.get(channel.id)
                    if target_id is None:
                        target_id = writer.register_channel(
                            TOPIC_MAP[channel.topic], "ros1", ros1_schema_id, channel.metadata
                        )
                    channel_ids[channel.id] = target_id
                else:
                    channel_ids[channel.id] = writer.register_channel(
                        channel.topic, channel.message_encoding,
                        schema_map[channel.schema_id], channel.metadata,
                    )

            count = converted = copied = 0
            for _, channel, message in reader.iter_messages():
                if limit is not None and count >= limit:
                    break
                if channel.message_encoding == "cdr" and channel.topic in TOPIC_MAP:
                    image = parse_ros2_image(message.data)
                    if image["encoding"] != "16UC1":
                        raise RuntimeError(f"topic {channel.topic}: only 16UC1 is supported")
                    if image["is_bigendian"] != 0:
                        raise RuntimeError(f"topic {channel.topic}: big-endian image is not supported")
                    if image["step"] != image["width"] * 2:
                        raise RuntimeError(f"topic {channel.topic}: unexpected row step")
                    expected_size = image["height"] * image["step"]
                    if len(image["data"]) != expected_size:
                        raise RuntimeError(f"topic {channel.topic}: truncated depth data")
                    png = encode_lossless_depth_png(image["data"], image["height"], image["width"])
                    writer.add_message(
                        channel_ids[channel.id], message.log_time,
                        serialize_ros1_compressed_depth(image, DEPTH_PREFIX + png),
                        message.publish_time, message.sequence,
                    )
                    converted_by_topic[channel.topic] += 1
                    converted += 1
                else:
                    writer.add_message(
                        channel_ids[channel.id], message.log_time, message.data,
                        message.publish_time, message.sequence,
                    )
                    copied += 1
                count += 1

            for metadata in reader.iter_metadata():
                writer.add_metadata(metadata.name, metadata.metadata)
            for attachment in reader.iter_attachments():
                writer.add_attachment(
                    attachment.create_time, attachment.log_time,
                    attachment.name, attachment.media_type, attachment.data,
                )
            writer.finish()

    result = {
        "message_count": count,
        "converted_count": converted,
        "copied_count": copied,
        "converted_by_topic": converted_by_topic,
    }
    if limit is None:
        verification = verify(source_path, output_path)
        if not verification["valid"]:
            raise RuntimeError(f"output verification failed: {json.dumps(verification)}")
        result["verification"] = verification
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("input", type=Path)
    parser.add_argument("output", type=Path, nargs="?")
    parser.add_argument("--inspect", action="store_true", help="classify depth channels and print JSON")
    parser.add_argument("--verify", action="store_true", help="verify source/output invariants and print JSON")
    parser.add_argument("--limit", type=int, help="debug only: write N messages without final verification")
    parser.add_argument("--no-compression", action="store_true", help="write chunks uncompressed")
    args = parser.parse_args()

    try:
        if args.inspect:
            print(json.dumps(inspect(args.input), sort_keys=True))
            return 0
        if args.verify:
            if args.output is None:
                parser.error("--verify requires OUTPUT")
            print(json.dumps(verify(args.input, args.output), sort_keys=True))
            return 0
        if args.output is None:
            parser.error("OUTPUT is required unless --inspect is used")
        print(json.dumps(convert(args.input, args.output, args.limit, args.no_compression), sort_keys=True))
        return 0
    except Exception as error:
        print(f"normalize depth failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
