# E2 multimodal conversion Job

The entrypoint converts an extracted Ego Portal E2 capture into:

- `output_bag.mcap`, containing Foxglove Protobuf H.264 video topics and ROS 2 Humble CDR IMU;
- `metadata.yaml`, using the rosbag2 metadata shape;
- `calibration.json`, assembled from the two camera parameter files;
- `processing_manifest.json`, containing output identities and conversion statistics.

The Orbit contract is exposed by `run_processing.py`. It validates the source size/checksum,
extracts tar files with traversal/link/device protections, accepts either root-level content or one
wrapper directory, and publishes the three conversion outputs plus the manifest to the output
binding.
