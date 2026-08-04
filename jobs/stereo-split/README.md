# Stereo split Job

The formal `stereo-split` image keeps the Orbit invocation contract stable while versioning the
contents of its processing manifest and MCAP output.

## Stable Orbit contract

The image entrypoint is `/app/run_processing.py`. It accepts the existing `--input`,
`--output-binding`, `--scratch`, source identity, processor image, kind, and generation arguments.
Successful runs publish these files to the output binding:

- `output_bag.mcap`
- `metadata.yaml`
- `processing_manifest.json`

## Output versions

| Manifest | Stereo topics | Schema | Statistics |
| --- | --- | --- | --- |
| v1 (legacy image) | `/decxin/left_rgb/compressed`, `/decxin/right_rgb/compressed` | `sensor_msgs/msg/CompressedImage` over CDR, JPEG payload | `left_images`, `right_images` |
| v2 (current image) | `/decxin/left_rgb/h264`, `/decxin/right_rgb/h264` | `foxglove.CompressedVideo` over Protobuf, H.264 Annex-B payload | `left_videos`, `right_videos` |

Both versions publish decoded IMU samples on `/decxin/imu`. The v2 converter removes the joined
`/decxin/rgb/compressed` source topic, replaces any existing `/decxin/imu` topic with samples
decoded from the joined image, and preserves all other unrelated input topics. Its manifest sets
`schema_version` to `2` and `output_format` to `stereo_h264`.

Keystone must be upgraded to a version that validates both manifest versions before selecting the
v2 image digest. In-flight executions keep their frozen image digest, and the configured image can
be rolled back to a v1 digest without changing the Orbit command or output bindings.

Calibration or other consumers of `output_bag.mcap` must support the v2 H.264 topics before the new
image is enabled for their workflow.
