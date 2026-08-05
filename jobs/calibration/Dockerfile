# syntax=docker/dockerfile:1.7
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

FROM ros:jazzy-ros-base

ENV DEBIAN_FRONTEND=noninteractive \
    MPLBACKEND=Agg \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

RUN sed -i \
        's|http://ports.ubuntu.com/ubuntu-ports/|https://mirrors.aliyun.com/ubuntu-ports/|g' \
        /etc/apt/sources.list.d/ubuntu.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        cmake \
        libboost-all-dev \
        libeigen3-dev \
        libgoogle-glog-dev \
        libopencv-dev \
        libsuitesparse-dev \
        libtbb-dev \
        libyaml-cpp-dev \
        pkg-config \
        python3-dev \
        python3-igraph \
        python3-matplotlib \
        python3-numpy \
        python3-opencv \
        python3-pip \
        python3-scipy \
    && rm -rf /var/lib/apt/lists/*

COPY jobs/calibration/requirements.txt /tmp/calibration-requirements.txt
ARG PYPI_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/
RUN --mount=type=cache,target=/root/.cache/pip \
    python3 -m pip install --break-system-packages \
        --ignore-installed \
        --index-url "${PYPI_INDEX_URL}" \
        --no-deps \
        --retries 10 \
        --timeout 120 \
        pyyaml==6.0.2 \
    && python3 -m pip install --break-system-packages \
        --index-url "${PYPI_INDEX_URL}" \
        --retries 10 \
        --timeout 120 \
        -r /tmp/calibration-requirements.txt

WORKDIR /workspace
COPY --from=archebase_calib . /workspace

ARG BUILD_JOBS=4
RUN BUILD_TYPE=Release JOBS="${BUILD_JOBS}" scripts/build_all.sh kalibr --clean

WORKDIR /app
COPY jobs/stereo-split/split_mcap_stereo_imu.py /app/split_mcap_stereo_imu.py
COPY jobs/calibration/preprocess_decxin.py /app/preprocess_decxin.py
COPY jobs/calibration/run_calibration.py /app/run_calibration.py

ARG ARCHEBASE_CALIB_REVISION=unknown
ENV ARCHEBASE_CALIB_REVISION=${ARCHEBASE_CALIB_REVISION}

RUN python3 -m compileall -q /app /workspace/calib_cli

ENTRYPOINT ["python3", "/app/run_calibration.py"]
CMD ["--help"]
