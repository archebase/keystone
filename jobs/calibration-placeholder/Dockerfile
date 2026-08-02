# syntax=docker/dockerfile:1.7
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

ARG PYTHON_IMAGE=python:3.11-slim-bookworm

FROM ${PYTHON_IMAGE}

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /app

COPY jobs/calibration-placeholder/run_calibration.py /app/run_calibration.py

RUN python3 -m compileall -q /app

ENTRYPOINT ["python3", "/app/run_calibration.py"]
