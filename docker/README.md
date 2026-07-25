<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Docker Development Environment

## Dockerfiles

| File | Purpose |
|------|---------|
| `Dockerfile` | Production build - multi-stage, minimal final image |
| `docker/Dockerfile.dev` | Development - mounts source code for live coding |

## Compose Files

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Infrastructure only (MySQL, MinIO, Redis, Adminer) |
| `docker-compose.dev.yml` | Development mode with volume mounts |
| `docker-compose.test.yml` | Local smoke-test environment |

## Development Mode

Start Keystone with source code mounted for live development:

```bash
docker compose -f docker/docker-compose.dev.yml up -d
```

Your code changes are reflected immediately. To rebuild inside container:

```bash
docker exec keystone-edge-dev go build ./cmd/keystone-edge
```

## Production Build

Build and run the production image:

```bash
# Build image
docker build -t keystone-edge:latest .

# Run with infrastructure
docker compose -f docker/docker-compose.yml up -d
docker run -d --name keystone-edge \
  --network docker_default \
  -p 8080:8080 \
  -p 8090:8090 \
  -p 8091:8091 \
  -p 50053:50053 \
  --env KEYSTONE_BIND_ADDR=:8080 \
  --env KEYSTONE_MYSQL_HOST=mysql \
  --env KEYSTONE_MINIO_ENDPOINT=http://minio:9000 \
  keystone-edge:latest
```

## Volcengine Container Registry

Log in with the temporary username and password shown by the Volcengine CR
console, then provide the CR instance domain and namespace when pushing:

```bash
export CR_REGISTRY=archebase-cr-cn-beijing.cr.volces.com
export CR_NAMESPACE=your-namespace

docker login "${CR_REGISTRY}"
make push CR_REGISTRY="${CR_REGISTRY}" CR_NAMESPACE="${CR_NAMESPACE}"
```

The resulting image is
`${CR_REGISTRY}/${CR_NAMESPACE}/keystone-edge:${IMAGE_TAG}`. Set
`CR_REPOSITORY` or `IMAGE_TAG` when the CR repository name or tag differs.

The production and development Dockerfiles pull base images directly from the
shared Volcengine CR `upstream` namespace:

- `archebase-cr-cn-beijing.cr.volces.com/upstream/golang:1.25-bookworm`
- `archebase-cr-cn-beijing.cr.volces.com/upstream/alpine:3.20`

Both mirrored images currently publish a `linux/amd64` manifest, so the
Dockerfiles explicitly target that platform.

Compose files also prefer Volcengine CR for mirrored upstream images:

- `archebase-cr-cn-beijing.cr.volces.com/upstream/mysql:8.4.10`
- `archebase-cr-cn-beijing.cr.volces.com/upstream/alpine:3.20`

MinIO uses the upstream Quay registry until `minio` and `mc` are mirrored into
Volcengine CR. Optional Redis/Adminer services are behind the `optional` compose
profile and point at the expected Volcengine CR mirror names; mirror these images
before enabling that profile:

- `archebase-cr-cn-beijing.cr.volces.com/upstream/minio:latest`
- `archebase-cr-cn-beijing.cr.volces.com/upstream/mc:latest`
- `archebase-cr-cn-beijing.cr.volces.com/upstream/redis:7-alpine`
- `archebase-cr-cn-beijing.cr.volces.com/upstream/adminer:latest`

Run `docker login "${CR_REGISTRY}"` before building when the local Docker
credential store does not already contain the CR robot credentials.

## Infrastructure Only

Start just the dependencies (MySQL, MinIO, etc.):

```bash
docker compose -f docker/docker-compose.yml up -d
```

Then run Keystone locally:

```bash
export $(cat docker/.env.example | xargs)
go run cmd/keystone-edge/main.go
```

## Services

| Service | Port | Description |
|---------|------|-------------|
| MySQL | 3306 | Database |
| MinIO API | 9000 | Object storage API |
| MinIO Console | 9001 | MinIO management UI |
| Redis | 6379 | Cache (optional) |
| Adminer | 9002 | Database management tool |
| Keystone Edge | 8080 | Main API service |
| Keystone Transfer | 8090 | Axon transfer WebSocket |
| Keystone Recorder | 8091 | Axon recorder WebSocket |
| Keystone DGW | 50053 | Optional DGW-compatible gRPC service |

## Access URLs

- **MinIO Console**: http://localhost:9001
  - Username: `minioadmin`
  - Password: `minioadmin`

- **Adminer**: http://localhost:9002
  - System: MySQL
  - Server: `mysql`
  - Username: `keystone`
  - Password: `keystone`
  - Database: `keystone`

- **Keystone Edge API**: http://localhost:8080
- **Swagger UI**: http://localhost:8080/swagger/index.html

## Quick Commands

```bash
# Stop all services
docker compose -f docker/docker-compose.dev.yml down

# Stop and remove data
docker compose -f docker/docker-compose.dev.yml down -v

# View logs
docker compose -f docker/docker-compose.dev.yml logs -f keystone-edge-dev

# Execute command in container
docker exec -it keystone-edge-dev sh
```

## Local Smoke Testing

The `docker-compose.test.yml` file can be used manually on a machine with a
Docker daemon:

```bash
docker compose -f docker/docker-compose.test.yml up -d --build
curl -f http://localhost:8080/api/v1/health
curl -f http://localhost:8080/swagger/doc.json
docker compose -f docker/docker-compose.test.yml down -v
```

GitHub Actions runs in an ARC/Kubernetes job container and does not run
docker-compose based integration tests.
