#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Convert ROS2 raw 16UC1 depth topics to ROS1 compressedDepth topics.

The conversion preserves depth values losslessly and copies every other MCAP
message unchanged. It is intended to normalize recordings whose Realsense depth
streams were captured as ROS2 ``sensor_msgs/Image`` instead of the ROS1
``sensor_msgs/CompressedImage`` format used by the existing dataset.

Example:
  python3 scripts/normalize_ros2_depth_to_ros1_compresseddepth.py \
    input.mcap output.mcap

Requires: mcap, opencv-python-headless, numpy
"""

import argparse
import struct
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Iterator, Tuple

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
COMPRESSED_FORMAT = b"16UC1; compressedDepth png"
# compressed_depth_image_transport puts three float32 parameters before the PNG.
# 0819 source is raw 16UC1, so all-zero parameters preserve the pixel values exactly.
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

    def align(self, size: int) -> None:
        # The source writer serializes these messages byte-packed without CDR field padding.
        pass

    def unpack(self, fmt: str, buf: bytes) -> Tuple:
        self.align(struct.calcsize(fmt))
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
    r = Reader()
    seq, sec, nsec = r.unpack("<III", data)
    frame_id = r.string(data)
    r.align(4)
    height, width = r.unpack("<II", data)
    encoding = r.string(data)
    (is_bigendian,) = r.unpack("<B", data)
    r.align(4)
    (step,) = r.unpack("<I", data)
    r.align(4)
    (array_len,) = r.unpack("<I", data)
    payload = data[r.pos:r.pos + array_len]
    if len(payload) != array_len:
        raise ValueError("truncated image data")
    return {
        "seq": seq,
        "sec": sec,
        "nsec": nsec,
        "frame_id": frame_id,
        "height": height,
        "width": width,
        "encoding": encoding,
        "is_bigendian": is_bigendian,
        "step": step,
        "data": payload,
    }


def encode_lossless_depth_png(data: bytes, height: int, width: int) -> bytes:
    depth = np.frombuffer(data, dtype="<u2").reshape(height, width)
    ok, png = cv2.imencode(".png", depth, [int(cv2.IMWRITE_PNG_COMPRESSION), 3])
    if not ok:
        raise RuntimeError("OpenCV failed to encode depth PNG")
    return png.tobytes()


def serialize_ros1_compressed_depth(image: Dict[str, object], png: bytes) -> bytes:
    frame_id = image["frame_id"].encode("utf-8")
    fmt = COMPRESSED_FORMAT
    # ROS1 serialization: Header, format string, uint8[] data. All fields are 4-byte aligned.
    size = 4 + 4 + 4 + 4 + len(frame_id) + 4 + len(fmt) + 4 + len(png)
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
    put_bytes(fmt)
    put_bytes(png)
    return bytes(out)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("input", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--limit", type=int, help="debug: write only N messages total")
    parser.add_argument("--no-compression", action="store_true", help="write chunks uncompressed")
    args = parser.parse_args()

    if args.input.resolve() == args.output.resolve():
        parser.error("input and output must be different files")
    args.output.parent.mkdir(parents=True, exist_ok=True)

    with args.input.open("rb") as source_file:
        reader = make_reader(source_file)
        header = reader.get_header()
        summary = reader.get_summary()
        old_schemas = dict(summary.schemas or {})
        old_channels = dict(summary.channels or {})

        # Register every source schema except ROS2 depth Image, plus the ROS1 target schema.
        # Reserve IDs first, then map each source channel to its newly assigned schema ID.
        temp_schema_map: Dict[int, int] = {}
        with args.output.open("wb") as output_file:
            writer = Writer(output_file)
            writer.start(profile=header.profile, library="convert-depth-lossless/1.0")
            ros1_schema_id = writer.register_schema("sensor_msgs/CompressedImage", "ros1msg", ROS1_SCHEMA)

            # Register source schemas in deterministic order, skipping only the source depth schema.
            for schema in old_schemas.values():
                if schema.name == "sensor_msgs/Image" and schema.encoding == "ros2msg":
                    continue
                temp_schema_map[schema.id] = writer.register_schema(schema.name, schema.encoding, schema.data)

            # Map source depth schema to target schema.
            depth_schema_ids = [sid for sid, s in old_schemas.items() if s.name == "sensor_msgs/Image" and s.encoding == "ros2msg"]
            if not depth_schema_ids:
                raise RuntimeError("source ROS2 Image schema not found")
            source_schema_id = depth_schema_ids[0]
            temp_schema_map[source_schema_id] = ros1_schema_id

            channel_ids = {}
            target_channel_ids = {}
            for channel in old_channels.values():
                if channel.message_encoding == "cdr" and channel.topic in TOPIC_MAP:
                    # One target channel per topic, sharing the ROS1 schema.
                    if channel.topic not in target_channel_ids:
                        target_channel_ids[channel.topic] = writer.register_channel(
                            TOPIC_MAP[channel.topic], "ros1", ros1_schema_id, channel.metadata
                        )
                    channel_ids[channel.id] = target_channel_ids[channel.topic]
                else:
                    channel_ids[channel.id] = writer.register_channel(
                        channel.topic,
                        channel.message_encoding,
                        temp_schema_map[channel.schema_id],
                        channel.metadata,
                    )

            count = 0
            converted = 0
            copied = 0
            for schema, channel, message in reader.iter_messages():
                if args.limit is not None and count >= args.limit:
                    break
                if channel.message_encoding == "cdr" and channel.topic in TOPIC_MAP:
                    image = parse_ros2_image(message.data)
                    if image["encoding"] != "16UC1":
                        raise RuntimeError(
                            f"topic {channel.topic}: only 16UC1 is supported, got {image['encoding']}"
                        )
                    if image["is_bigendian"] != 0:
                        raise RuntimeError(f"topic {channel.topic}: big-endian image is not supported")
                    if image["step"] != image["width"] * 2:
                        raise RuntimeError(
                            f"topic {channel.topic}: expected step={image['width'] * 2}, got {image['step']}"
                        )
                    expected = image["height"] * image["step"]
                    if len(image["data"]) != expected:
                        raise RuntimeError(
                            f"topic {channel.topic}: expected {expected} data bytes, got {len(image['data'])}"
                        )
                    png = encode_lossless_depth_png(image["data"], image["height"], image["width"])
                    payload = serialize_ros1_compressed_depth(image, DEPTH_PREFIX + png)
                    writer.add_message(
                        channel_ids[channel.id], message.log_time, payload,
                        message.publish_time, message.sequence,
                    )
                    converted += 1
                else:
                    writer.add_message(
                        channel_ids[channel.id], message.log_time, message.data,
                        message.publish_time, message.sequence,
                    )
                    copied += 1
                count += 1
                if count % 1000 == 0:
                    print(f"\r{count} messages ({converted} converted, {copied} copied)", end="", flush=True)

            for metadata in reader.iter_metadata():
                writer.add_metadata(metadata.name, metadata.metadata)
            for attachment in reader.iter_attachments():
                writer.add_attachment(
                    attachment.create_time, attachment.log_time,
                    attachment.name, attachment.media_type, attachment.data,
                )
            writer.finish()
            print(f"\nWrote {count} messages: {converted} converted, {copied} copied")
            print(f"Output: {args.output} ({args.output.stat().st_size / 1024 / 1024:.1f} MiB)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
