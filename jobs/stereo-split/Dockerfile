# syntax=docker/dockerfile:1.7
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

ARG PYTHON_IMAGE=python:3.11-slim-bookworm

FROM ${PYTHON_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      libglib2.0-0 \
      libgomp1 \
    && rm -rf /var/lib/apt/lists/*

COPY jobs/stereo-split/requirements.txt /tmp/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install -r /tmp/requirements.txt

COPY jobs/stereo-split/split_mcap_stereo_imu.py /app/split_mcap_stereo_imu.py
COPY jobs/stereo-split/run_processing.py /app/run_processing.py

RUN python3 -m compileall -q /app

ENTRYPOINT ["python3", "/app/run_processing.py"]
