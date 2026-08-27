# syntax=docker/dockerfile:1.7
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

FROM python:3.11-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1

ARG DEBIAN_MIRROR=http://deb.debian.org/debian
ARG DEBIAN_SECURITY_MIRROR=http://deb.debian.org/debian-security

RUN sed -i \
      -e "s|http://deb.debian.org/debian-security|${DEBIAN_SECURITY_MIRROR}|g" \
      -e "s|http://deb.debian.org/debian|${DEBIAN_MIRROR}|g" \
      /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::ForceIPv4=true -o Acquire::Retries=3 update \
    && apt-get -o Acquire::ForceIPv4=true -o Acquire::Retries=3 install -y --no-install-recommends \
         ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && ffmpeg -hide_banner -encoders 2>&1 | grep -q libx264
WORKDIR /app
COPY jobs/e2-multimodal-conversion/requirements.txt /tmp/requirements.txt
ARG PYPI_INDEX_URL=https://pypi.org/simple
RUN pip install --no-cache-dir --index-url "${PYPI_INDEX_URL}" --timeout 120 --retries 5 -r /tmp/requirements.txt
COPY jobs/e2-multimodal-conversion/e2_converter.py jobs/e2-multimodal-conversion/run_processing.py /app/
RUN python3 -m compileall -q /app
ENTRYPOINT ["python3", "/app/run_processing.py"]
