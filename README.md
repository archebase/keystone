# Keystone Edge

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
swag init

# Format docs
swag fmt
```

### Build

```bash
# Build binary
go build -o bin/keystone-edge ./cmd/keystone-edge

# Run with custom config
KEYSTONE_BIND_ADDR=:9090 ./bin/keystone-edge
```

## License

MIT
