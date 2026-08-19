# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

FROM --platform=linux/amd64 archebase-cr-cn-beijing.cr.volces.com/upstream/golang:1.25-bookworm AS builder

ARG BUILD_TIME=unknown
ARG DEBIAN_MIRROR=http://mirrors.aliyun.com/debian
ARG DEBIAN_SECURITY_MIRROR=http://mirrors.aliyun.com/debian-security
ARG GOPROXY=https://goproxy.cn,direct
ARG VERSION=dev

ENV CGO_ENABLED=0 \
    GO111MODULE=on \
    GOPROXY=${GOPROXY}

WORKDIR /build

RUN rm -f /etc/apt/sources.list.d/debian.sources && \
    printf 'deb %s bookworm main\n' "$DEBIAN_MIRROR" > /etc/apt/sources.list && \
    printf 'deb %s bookworm-updates main\n' "$DEBIAN_MIRROR" >> /etc/apt/sources.list && \
    printf 'deb %s bookworm-security main\n' "$DEBIAN_SECURITY_MIRROR" >> /etc/apt/sources.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends git && \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

RUN go install github.com/swaggo/swag/cmd/swag@v1.16.6

COPY . .

RUN swag init --parseDependency --parseInternal \
        -g cmd/keystone-edge/main.go \
        -o docs && \
    GOOS=linux GOARCH=amd64 go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME}" \
        -o /out/keystone-edge \
        ./cmd/keystone-edge

FROM --platform=linux/amd64 archebase-cr-cn-beijing.cr.volces.com/upstream/alpine:3.20

ARG ALPINE_MIRROR=http://mirrors.aliyun.com/alpine

RUN printf '%s/v3.20/main\n' "$ALPINE_MIRROR" > /etc/apk/repositories && \
    printf '%s/v3.20/community\n' "$ALPINE_MIRROR" >> /etc/apk/repositories && \
    apk add --no-cache ca-certificates tzdata python3 py3-numpy py3-opencv py3-pip && \
    python3 -m pip install --no-cache-dir --break-system-packages \
        'mcap==1.4.0' \
        'zstandard==0.25.0' && \
    addgroup -g 1000 keystone && \
    adduser -D -u 1000 -G keystone -h /app keystone

ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder --chown=keystone:keystone /out/keystone-edge /app/keystone-edge
COPY --chown=keystone:keystone scripts/normalize_ros2_depth_to_ros1_compresseddepth.py /app/scripts/normalize_ros2_depth_to_ros1_compresseddepth.py

USER keystone

EXPOSE 8080 8090 8091 50053

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["/app/keystone-edge"]
