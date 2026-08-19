#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Normalize ZJ-WA1-D MCAP depth streams to the current ROS1 target contract.

Target contract:
  * head depth: ROS1 ``sensor_msgs/CompressedImage`` on ``.../compressedDepth``
  * chest/up depth: ROS1 ``sensor_msgs/Image`` on ``.../image_raw``

The converter preserves depth values losslessly and copies every unrelated MCAP
message unchanged. ``--inspect`` classifies an input without writing a file;
``--verify`` validates conversion invariants and emits a JSON result.

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

HEAD_RAW = "/zj_humanoid/sensor/realsense_head/aligned_depth_to_color/image_raw"
HEAD_COMPRESSED = HEAD_RAW + "/compressedDepth"
UP_RAW = "/zj_humanoid/sensor/realsense_up/aligned_depth_to_color/image_raw"
UP_COMPRESSED = UP_RAW + "/compressedDepth"
COMPRESSED_FORMAT = b"16UC1; compressedDepth png"
DEPTH_PREFIX = struct.pack("<fff", 0.0, 0.0, 0.0)

ROS1_COMPRESSED_SCHEMA = b"""# This message contains a compressed image

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
class ByteReader:
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


@dataclass
class Ros1Reader:
    pos: int = 0

    def unpack(self, fmt: str, buf: bytes) -> Tuple:
        value = struct.unpack_from(fmt, buf, self.pos)
        self.pos += struct.calcsize(fmt)
        return value

    def string(self, buf: bytes) -> str:
        (size,) = self.unpack("<I", buf)
        value = buf[self.pos:self.pos + size]
        if len(value) != size:
            raise ValueError("truncated ROS1 string")
        self.pos += size
        return value.decode("utf-8")


def parse_packed_ros2_image(data: bytes) -> Dict[str, object]:
    # Axon 0.4.0 serialized these ROS2-schema Image messages byte-packed rather
    # than using normal CDR alignment, so decode the historical wire format exactly.
    r = ByteReader()
    seq = r.unpack("<I", data)[0]
    sec = r.unpack("<i", data)[0]
    nsec = r.unpack("<I", data)[0]
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


def parse_ros1_image(data: bytes) -> Dict[str, object]:
    r = Ros1Reader()
    seq = r.unpack("<I", data)[0]
    sec = r.unpack("<i", data)[0]
    nsec = r.unpack("<I", data)[0]
    frame_id = r.string(data)
    height, width = r.unpack("<II", data)
    encoding = r.string(data)
    (is_bigendian,) = r.unpack("<B", data)
    (step,) = r.unpack("<I", data)
    (array_len,) = r.unpack("<I", data)
    payload = data[r.pos:r.pos + array_len]
    if len(payload) != array_len:
        raise ValueError("truncated ROS1 image data")
    return {
        "seq": seq, "sec": sec, "nsec": nsec, "frame_id": frame_id,
        "height": height, "width": width, "encoding": encoding,
        "is_bigendian": is_bigendian, "step": step, "data": payload,
    }


def parse_ros1_compressed_image(data: bytes) -> Dict[str, object]:
    r = Ros1Reader()
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
            counts = summary.statistics.channel_message_counts if summary.statistics else {}
            channels[channel.topic] = {
                "count": int(counts.get(channel.id, 0)),
                "message_encoding": channel.message_encoding,
                "schema": schema.name if schema else "",
                "schema_encoding": schema.encoding if schema else "",
            }
        total = summary.statistics.message_count if summary.statistics else sum(x["count"] for x in channels.values())
        return {"total_count": int(total), "channels": channels}


def topic_info(info: Dict[str, object], topic: str) -> Dict[str, object]:
    return (info["channels"] or {}).get(topic, {  # type: ignore[union-attr]
        "count": 0, "message_encoding": "", "schema": "", "schema_encoding": "",
    })


def channel_matches(info: Dict[str, object], topic: str, *, count: bool = True, **expected: str) -> bool:
    value = topic_info(info, topic)
    return (value["count"] > 0 or not count) and all(
        value[key] == expected_value for key, expected_value in expected.items()
    )


def packed_ros2_raw(info: Dict[str, object], topic: str) -> bool:
    return channel_matches(
        info, topic, message_encoding="cdr", schema="sensor_msgs/Image", schema_encoding="ros2msg"
    )


def ros1_raw(info: Dict[str, object], topic: str) -> bool:
    return channel_matches(
        info, topic, message_encoding="ros1", schema="sensor_msgs/Image", schema_encoding="ros1msg"
    )


def ros1_compressed(info: Dict[str, object], topic: str) -> bool:
    return channel_matches(
        info, topic, message_encoding="ros1", schema="sensor_msgs/CompressedImage", schema_encoding="ros1msg"
    )


def inspect(path: Path) -> Dict[str, object]:
    info = summary_info(path)
    state = {
        "head_raw_packed_ros2": packed_ros2_raw(info, HEAD_RAW),
        "head_compressed_ros1": ros1_compressed(info, HEAD_COMPRESSED),
        "up_raw_packed_ros2": packed_ros2_raw(info, UP_RAW),
        "up_raw_ros1": ros1_raw(info, UP_RAW),
        "up_compressed_ros1": ros1_compressed(info, UP_COMPRESSED),
    }
    if state["head_compressed_ros1"] and state["up_compressed_ros1"]:
        status = "already_target"
    elif state["head_raw_packed_ros2"] and state["up_raw_packed_ros2"]:
        status = "requires_normalization"
    elif state["head_compressed_ros1"] and state["up_raw_ros1"]:
        status = "requires_chest_normalization"
    else:
        status = "unsupported"
    return {
        "status": status,
        "total_count": info["total_count"],
        "depth_channels": state,
        "channels": info["channels"],
    }


def encode_lossless_depth_png(data: bytes, height: int, width: int) -> bytes:
    depth = np.frombuffer(data, dtype="<u2").reshape(height, width)
    ok, png = cv2.imencode(".png", depth, [int(cv2.IMWRITE_PNG_COMPRESSION), 3])
    if not ok:
        raise RuntimeError("OpenCV failed to encode depth PNG")
    return png.tobytes()


class Ros1Writer:
    def __init__(self) -> None:
        self.out = bytearray()

    def put(self, fmt: str, *values: object) -> None:
        self.out.extend(struct.pack(fmt, *values))

    def put_bytes(self, value: bytes) -> None:
        self.put("<I", len(value))
        self.out.extend(value)




def serialize_ros1_compressed_depth(image: Dict[str, object], png: bytes) -> bytes:
    writer = Ros1Writer()
    writer.put("<I", image["seq"])
    writer.put("<i", image["sec"])
    writer.put("<I", image["nsec"])
    writer.put_bytes(str(image["frame_id"]).encode("utf-8"))
    writer.put_bytes(COMPRESSED_FORMAT)
    writer.put_bytes(DEPTH_PREFIX + png)
    return bytes(writer.out)


def validate_packed_depth(image: Dict[str, object], topic: str) -> None:
    if image["encoding"] != "16UC1":
        raise RuntimeError(f"topic {topic}: only 16UC1 is supported")
    if image["is_bigendian"] != 0:
        raise RuntimeError(f"topic {topic}: big-endian image is not supported")
    if image["step"] != image["width"] * 2:
        raise RuntimeError(f"topic {topic}: unexpected row step")
    expected_size = image["height"] * image["step"]
    if len(image["data"]) != expected_size:
        raise RuntimeError(f"topic {topic}: truncated depth data")


def first_message(path: Path, topic: str) -> bytes | None:
    with path.open("rb") as stream:
        for _, channel, message in make_reader(stream).iter_messages(topics=[topic]):
            return message.data
    return None


def sample_equal(source: Path, output: Path, source_topic: str, output_topic: str) -> bool:
    source_data = first_message(source, source_topic)
    output_data = first_message(output, output_topic)
    if source_data is None or output_data is None:
        return False

    if source_topic in (HEAD_RAW, UP_RAW):
        source_info = inspect(source)
        raw = (parse_packed_ros2_image(source_data) if source_info["depth_channels"][  # type: ignore[index]
                "head_raw_packed_ros2" if source_topic == HEAD_RAW else "up_raw_packed_ros2"]
               else parse_ros1_image(source_data))
        compressed = parse_ros1_compressed_image(output_data)
        decoded = cv2.imdecode(
            np.frombuffer(compressed["data"][len(DEPTH_PREFIX):], dtype=np.uint8),
            cv2.IMREAD_UNCHANGED,
        )
        expected = np.frombuffer(raw["data"], dtype="<u2").reshape(raw["height"], raw["width"])  # type: ignore[attr-defined]
        return decoded is not None and decoded.dtype == expected.dtype and bool(np.array_equal(decoded, expected))
    return source_data == output_data


def verify(source: Path, output: Path) -> Dict[str, object]:
    source_info = inspect(source)
    output_info = inspect(output)
    source_status = source_info["status"]
    checks = {
        "source_requires_normalization": source_status in (
            "requires_normalization", "requires_chest_normalization"
        ),
        "output_target": output_info["status"] == "already_target",
        "total_count_equal": source_info["total_count"] == output_info["total_count"],
    }
    if source_status == "requires_normalization":
        pairs = ((HEAD_RAW, HEAD_COMPRESSED), (UP_RAW, UP_COMPRESSED))
    elif source_status == "requires_chest_normalization":
        pairs = ((HEAD_COMPRESSED, HEAD_COMPRESSED), (UP_RAW, UP_COMPRESSED))
    else:
        pairs = ()

    topic_counts = {}
    for source_topic, output_topic in pairs:
        source_count = topic_info(source_info, source_topic)["count"]
        output_count = topic_info(output_info, output_topic)["count"]
        checks[f"count_equal:{output_topic}"] = source_count == output_count
        topic_counts[output_topic] = {"source": source_count, "output": output_count}
        checks[f"lossless_sample:{output_topic}"] = sample_equal(
            source, output, source_topic, output_topic
        )
    return {
        "source_status": source_status,
        "output_status": output_info["status"],
        "source_total_count": source_info["total_count"],
        "output_total_count": output_info["total_count"],
        "topic_counts": topic_counts,
        "checks": checks,
        "valid": all(checks.values()),
    }


def convert(source_path: Path, output_path: Path, limit: int | None, no_compression: bool) -> Dict[str, object]:
    if source_path.resolve() == output_path.resolve():
        raise RuntimeError("input and output must be different files")
    admission = inspect(source_path)
    if admission["status"] not in ("requires_normalization", "requires_chest_normalization"):
        raise RuntimeError(f"input depth format is {admission['status']}, expected normalization")
    output_path.parent.mkdir(parents=True, exist_ok=True)

    with source_path.open("rb") as source_file:
        reader = make_reader(source_file)
        header = reader.get_header()
        summary = reader.get_summary()
        old_schemas = dict(summary.schemas or {})
        old_channels = dict(summary.channels or {})

        with output_path.open("wb") as output_file:
            compression = CompressionType.NONE if no_compression else CompressionType.ZSTD
            writer = Writer(output_file, compression=compression)
            writer.start(profile=header.profile, library="normalize-depth/2.0")
            compressed_schema_id = writer.register_schema(
                "sensor_msgs/CompressedImage", "ros1msg", ROS1_COMPRESSED_SCHEMA
            )
            copied_schema_ids: Dict[int, int] = {}
            def copied_schema_id(schema_id: int) -> int:
                if schema_id not in copied_schema_ids:
                    schema = old_schemas[schema_id]
                    copied_schema_ids[schema_id] = writer.register_schema(
                        schema.name, schema.encoding, schema.data
                    )
                return copied_schema_ids[schema_id]

            channel_ids: Dict[int, int] = {}
            for channel in old_channels.values():
                if channel.topic == HEAD_RAW and channel.message_encoding == "cdr":
                    channel_ids[channel.id] = writer.register_channel(
                        HEAD_COMPRESSED, "ros1", compressed_schema_id, channel.metadata
                    )
                elif channel.topic == UP_RAW:
                    channel_ids[channel.id] = writer.register_channel(
                        UP_COMPRESSED, "ros1", compressed_schema_id, channel.metadata
                    )
                else:
                    channel_ids[channel.id] = writer.register_channel(
                        channel.topic, channel.message_encoding,
                        copied_schema_id(channel.schema_id), channel.metadata,
                    )

            count = converted_head = converted_up = copied = 0
            for _, channel, message in reader.iter_messages():
                if limit is not None and count >= limit:
                    break
                if channel.topic == HEAD_RAW and channel.message_encoding == "cdr":
                    image = parse_packed_ros2_image(message.data)
                    validate_packed_depth(image, channel.topic)
                    png = encode_lossless_depth_png(image["data"], image["height"], image["width"])
                    writer.add_message(
                        channel_ids[channel.id], message.log_time,
                        serialize_ros1_compressed_depth(image, png),
                        message.publish_time, message.sequence,
                    )
                    converted_head += 1
                elif channel.topic == UP_RAW:
                    image = (parse_packed_ros2_image(message.data)
                             if channel.message_encoding == "cdr" else parse_ros1_image(message.data))
                    validate_packed_depth(image, channel.topic)
                    png = encode_lossless_depth_png(image["data"], image["height"], image["width"])
                    writer.add_message(
                        channel_ids[channel.id], message.log_time,
                        serialize_ros1_compressed_depth(image, png), message.publish_time, message.sequence,
                    )
                    converted_up += 1
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
        "converted_count": converted_head + converted_up,
        "converted_head_to_compresseddepth": converted_head,
        "converted_up_to_compresseddepth": converted_up,
        "copied_count": copied,
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
