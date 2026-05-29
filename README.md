<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Keystone Edge

[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![CI](https://github.com/ArcheBase/keystone/actions/workflows/ci.yml/badge.svg)](https://github.com/ArcheBase/keystone/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Mulan%20PSL%20v2-blue)](LICENSE)

A backend for edge data collection scenarios.

## Features

- **Go-only implementation** - No Python dependencies
- **MySQL** - Primary database storage
- **MinIO** - S3-compatible object storage
- **SQLite** - Embedded queue for data persistence
- **State machine** - Task lifecycle management
- **QA engine** - Embedded quality assessment
- **Sync worker** - Cloud synchronization
- **Swagger UI** - Interactive API documentation

## Prerequisites

- Go 1.24+
- Docker & Docker Compose (for development)

## Quick Start

### Local Development

```bash
# 1. Copy environment configuration
cp .env.example .env

# 2. Start the server
./run-dev.sh

# API: http://localhost:8080
# Swagger UI: http://localhost:8080/swagger/index.html
```

### Docker Compose

```bash
# Start all services (MySQL, MinIO, Redis, Adminer)
docker-compose -f docker/docker-compose.yml up -d

# View logs
docker-compose -f docker/docker-compose.yml logs -f

# Stop services
docker-compose -f docker/docker-compose.yml down
```

## Available Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /api/v1/health` | Health check |
| `GET /swagger/*` | Swagger UI |
| `GET /api/v1/swagger.json` | OpenAPI spec |

## Configuration

Configuration is loaded from environment variables. See [`docker/.env.example`](docker/.env.example) for all available options.

### Key Variables

| Variable | Default | Description |
|----------|----------|-------------|
| `KEYSTONE_BIND_ADDR` | `:8080` | Server bind address |
| `KEYSTONE_MINIO_ENDPOINT` | `http://localhost:9000` | MinIO endpoint |
| `KEYSTONE_MYSQL_HOST` | `localhost` | MySQL host |
| `KEYSTONE_MYSQL_PASSWORD` | *required* | MySQL password |
| `KEYSTONE_SYNC_ENABLED` | `true` | Enable cloud sync capability, worker, and manual sync APIs when cloud endpoints and credentials are configured |
| `KEYSTONE_SYNC_AUTO_SCAN_ENABLED` | `false` | Enable periodic automatic discovery of newly eligible approved unsynced episodes |

### Cloud Sync Credentials

When cloud sync is enabled, `KEYSTONE_CLOUD_API_KEY` is required. Keystone treats
this value as an opaque credential issued by the cloud platform and forwards it
to `AuthService.ExchangeCredential` as `credential_base64`. Keystone does not
decode, split, validate, or derive `site_id` / secret values from this key; the
cloud AuthService owns credential interpretation and validation.

Cloud sync capability and automatic scheduling are separate. Keep
`KEYSTONE_SYNC_ENABLED=true` when admins should be able to manually sync data to
cloud. Leave `KEYSTONE_SYNC_AUTO_SCAN_ENABLED=false` for the default manual-only
mode, where newly recorded or newly approved episodes remain local until an
admin triggers single-episode sync or an explicit batch scan. Set
`KEYSTONE_SYNC_AUTO_SCAN_ENABLED=true` only when the site should automatically
queue every newly eligible approved unsynced episode.

## Project Structure

```
internal/
├── api/handlers/    # HTTP handlers with Swagger annotations
├── config/          # Environment-based configuration
├── server/          # HTTP server (Gin)
└── ...
docs/
├── swagger.json     # Generated OpenAPI spec
└── swagger.go       # Swagger template
```

## Development

### Generate Swagger Docs

```bash
# Install swag CLI
go install github.com/swaggo/swag/cmd/swag@latest

# Generate docs
swag init -g cmd/keystone-edge/main.go -o docs

# Format docs
swag fmt
```

### Code Formatting

```bash
# Format code using go fmt
go fmt ./...

# Or use gofmt for more options
gofmt -w .
```

### Code Linting

```bash

# Run linter
golangci-lint run

# Run linter with auto-fix
golangci-lint run --fix
```

### Build

```bash
# Build binary
go build -o bin/keystone-edge ./cmd/keystone-edge

# Run with custom config
KEYSTONE_BIND_ADDR=:9090 ./bin/keystone-edge
```

## License

This project is licensed under the [Mulan PSL v2](https://opensource.org/licenses/MulanPSL-2.0).

#### Verify Compliance

```bash
# Using Makefile
make license

# Or using the script directly
./scripts/ci-reuse-local.sh

# Generate SPDX SBOM
./scripts/ci-reuse-local.sh --sbom
```

#### Adding License to New Files

When creating new files, use the following header:

```go
// SPDX-FileCopyrightText: Copyright (c) 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0
```

For more information, see the [REUSE Tutorial](https://reuse.readthedocs.io/en/latest/).
