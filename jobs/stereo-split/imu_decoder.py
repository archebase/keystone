#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Decode DECXIN IMU samples embedded in joined JPEG scan lines."""

from __future__ import annotations

from dataclasses import dataclass
import numpy as np


K_GROUP_BYTES = 16
K_CODE_CELL = 8
K_LINE_SEARCH_OFFSET = 3
DEVICE_ICM42688 = 1


@dataclass
class ImuSample:
    timestamp_us: int = 0
    ax_mg: float = 0.0
    ay_mg: float = 0.0
    az_mg: float = 0.0
    gx_dps: float = 0.0
    gy_dps: float = 0.0
    gz_dps: float = 0.0


@dataclass
class ImuFrame:
    device_type: list[int]
    device_group_count: list[int]
    samples: list[ImuSample]


def decode_imu_from_bgr(bgr: np.ndarray) -> ImuFrame | None:
    candidates = (
        (K_LINE_SEARCH_OFFSET - 1, K_CODE_CELL),
        (bgr.shape[0] - K_LINE_SEARCH_OFFSET, -K_CODE_CELL),
        (K_LINE_SEARCH_OFFSET, K_CODE_CELL),
        (bgr.shape[0] - K_LINE_SEARCH_OFFSET - 1, -K_CODE_CELL),
    )
    for first_row, row_step in candidates:
        decoded = _decode_rows(bgr, first_row, row_step)
        if decoded and decoded.samples:
            return decoded
    return None


def _decode_rows(bgr: np.ndarray, first_row: int, row_step: int) -> ImuFrame | None:
    frame_bytes = bytearray()
    line_index = 0
    while line_index < K_GROUP_BYTES:
        frame_bytes.extend(_decode_line(bgr, first_row + line_index * row_step))
        if len(frame_bytes) >= K_GROUP_BYTES:
            break
        line_index += 1
    if len(frame_bytes) < K_GROUP_BYTES:
        return None
    result = _decode_header(frame_bytes[:K_GROUP_BYTES])
    total_size = (1 + sum(result.device_group_count)) * K_GROUP_BYTES
    line_index += 1
    while len(frame_bytes) < total_size:
        row = first_row + line_index * row_step
        if row < 0 or row >= bgr.shape[0]:
            break
        frame_bytes.extend(_decode_line(bgr, row))
        line_index += 1
    if len(frame_bytes) < total_size:
        return None
    group_index = 1
    for device_type, count in zip(result.device_type, result.device_group_count):
        for index in range(count):
            start = (group_index + index) * K_GROUP_BYTES
            if device_type == DEVICE_ICM42688:
                result.samples.append(_decode_icm42688(frame_bytes[start : start + K_GROUP_BYTES]))
        group_index += count
    return result


def _decode_line(bgr: np.ndarray, row: int) -> bytes:
    if row < 0 or row >= bgr.shape[0]:
        return b""
    line = bgr[row]
    flat = line.reshape(-1)
    if any(flat[index] <= 220 for index in range(4)):
        return b""
    x = 0
    while x < bgr.shape[1]:
        if int(line[x, 1]) < 220:
            x += K_CODE_CELL // 2
            break
        x += 1
    bits: list[int] = []
    while x + 1 < bgr.shape[1]:
        value = int(line[x, 1]) + int(line[x + 1, 1])
        if value < 100:
            bits.append(0)
        elif value < 440:
            bits.append(1)
        else:
            break
        x += K_CODE_CELL
    output = bytearray(len(bits) // 8)
    for index in range(len(output)):
        output[index] = sum(bits[index * 8 + bit] << bit for bit in range(8))
    return bytes(output)


def _decode_header(data: bytes | bytearray) -> ImuFrame:
    if data[:8] == data[8:16]:
        return ImuFrame([DEVICE_ICM42688, 0, 0, 0, 0], [11, 0, 0, 0, 0], [])
    return ImuFrame(
        [
            ((data[0] & 0x0F) << 4) | ((data[1] & 0xF0) >> 4),
            data[2],
            ((data[3] & 0x0F) << 4) | ((data[4] & 0xF0) >> 4),
            data[5],
            ((data[6] & 0x0F) << 4) | ((data[7] & 0xF0) >> 4),
        ],
        [data[1] & 0x0F, data[3] >> 4, data[4] & 0x0F, data[6] >> 4, data[7] & 0x0F],
        [],
    )


def _decode_icm42688(data: bytes | bytearray) -> ImuSample:
    values = [int.from_bytes(data[offset : offset + 2], "big", signed=True) for offset in range(4, 16, 2)]
    sample = ImuSample(timestamp_us=int.from_bytes(data[:4], "big"))
    ax, ay, az, gx, gy, gz = values
    if (ax == -1 and ay == -1 and az == -1) or gx == -32768:
        return sample
    acceleration_scale = 4000.0 / 32768.0
    gyro_scale = 1000.0 / 32768.0
    sample.ax_mg, sample.ay_mg, sample.az_mg = (value * acceleration_scale for value in (ax, ay, az))
    sample.gx_dps, sample.gy_dps, sample.gz_dps = (value * gyro_scale for value in (gx, gy, gz))
    return sample
