# syntax=docker/dockerfile:1.7
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

ARG PYTHON_IMAGE=python:3.11-slim-bookworm
FROM ${PYTHON_IMAGE}

ARG DEBIAN_MIRROR=http://deb.debian.org/debian
ARG DEBIAN_SECURITY_MIRROR=http://deb.debian.org/debian-security

ENV DEBIAN_FRONTEND=noninteractive \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

RUN sed -i \
      -e "s|http://deb.debian.org/debian-security|${DEBIAN_SECURITY_MIRROR}|g" \
      -e "s|http://deb.debian.org/debian|${DEBIAN_MIRROR}|g" \
      /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::ForceIPv4=true -o Acquire::Retries=3 update \
    && apt-get -o Acquire::ForceIPv4=true -o Acquire::Retries=3 install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && ffmpeg -hide_banner -encoders 2>&1 | grep -q libx264

ARG PYPI_INDEX_URL=https://pypi.org/simple
WORKDIR /app

COPY jobs/stereo-split-convert/requirements.txt /tmp/requirements.txt
RUN pip install --index-url "${PYPI_INDEX_URL}" --timeout 120 --retries 5 -r /tmp/requirements.txt

COPY jobs/stereo-split-convert/convert_mcap_stereo_h264.py /app/convert_mcap_stereo_h264.py
COPY jobs/stereo-split-convert/imu_decoder.py /app/imu_decoder.py
COPY jobs/stereo-split-convert/run_processing.py /app/run_processing.py

RUN python3 -m compileall -q /app
ENTRYPOINT ["python3", "/app/run_processing.py"]
