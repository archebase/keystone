<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# E6 multimodal conversion Job

The entrypoint converts an extracted Ego Portal E6 capture into:

- `output_bag.mcap`, containing Foxglove Protobuf H.264 video topics and ROS 2 Humble CDR IMU;
- `metadata.yaml`, using the rosbag2 metadata shape;
- `calibration.json`, a standard `archebase.calibration` document assembled from
  `camera_params_rgb.json` and `imu_calibration.json`;
- `processing_manifest.json`, containing output identities, calibration metadata, and conversion statistics.

The output contract is identical to the E2 job (`e2-multimodal-conversion`): the
same two `/camera/{left,right}/image/h264` topics, the same `/imu/data`, and the
same manifest shape, because Keystone's conversion QA checks exactly that
contract for both pipelines.

## Input layout

E6 writes a flat capture. A capture wrapped in a single directory is tolerated,
anything deeper is rejected.

| file | contents |
|---|---|
| `rgb.mp4` | HEVC, the RGB stereo pair side by side (2 × 1920 × 1200) |
| `rgb_metainfo.csv` | one row per encoded frame; `mid_exposure_utc_ns` is the frame time |
| `accel.csv`, `gyro.csv` | `timestamp_ns,x,y,z` |
| `camera_params_rgb.json` | the RGB pair's intrinsics and extrinsics (`eye`: left/right) |
| `imu_calibration.json` | IMU noise terms and the per-camera time alignment |

`tracking.mp4`, `ctrl.mp4`, `Preview/`, `audio.m4a` and the pose/hand/controller
CSVs are deliberately left unconverted for now: Keystone's QA contract checks
exactly the two video topics and `/imu/data`, and no consumer reads the rest yet.

## Conversion

The two eyes are cropped out of the side-by-side HEVC source (`crop=iw/2:ih:0:0`
and `crop=iw/2:ih:iw/2:0`) and re-encoded to H.264 with libx264, one access unit
per recorded frame, stamped with that frame's `mid_exposure_utc_ns`. Frame pacing
is passed through rather than forced to a nominal rate: an output frame rate
would duplicate frames across capture gaps and break the one-to-one mapping
between encoded access units and the recorded exposure timestamps. accel and gyro
are merged by nearest timestamp into `sensor_msgs/msg/Imu`.

## Tests

```bash
python -m pytest jobs/e6-multimodal-conversion/tests
```

The end-to-end case builds a real side-by-side HEVC capture with ffmpeg and runs
the converter over it, so ffmpeg must be on PATH (the Job image installs it).
