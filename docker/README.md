# Docker Development Environment

## Dockerfiles

| File | Purpose |
|------|---------|
| `Dockerfile` | Production build - multi-stage, minimal final image |
| `Dockerfile.dev` | Development - mounts source code for live coding |

## Compose Files

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Infrastructure only (MySQL, MinIO, Redis, Adminer) |
| `docker-compose.dev.yml` | Development mode with volume mounts |
| `docker-compose.test.yml` | CI/CD testing with automated tests |

## Development Mode

Start Keystone with source code mounted for live development:

```bash
docker compose -f docker-compose.dev.yml up -d
```

Your code changes are reflected immediately. To rebuild inside container:

```bash
docker exec keystone-edge-dev go build ./cmd/keystone-edge
```

## Production Build

Build and run the production image:

```bash
# Build image
docker build -f docker/Dockerfile -t keystone-edge:latest .

# Run with infrastructure
docker compose -f docker-compose.yml up -d
docker run -d --name keystone-edge \
  --network keystone_keystone-net \
  -p 8080:8080 \
  --env KEYSTONE_BIND_ADDR=:8080 \
  --env KEYSTONE_MYSQL_HOST=mysql \
  --env KEYSTONE_MINIO_ENDPOINT=http://minio:9000 \
  keystone-edge:latest
```

## Infrastructure Only

Start just the dependencies (MySQL, MinIO, etc.):

```bash
docker compose up -d
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
| Adminer | 8081 | Database management tool |
| Keystone Edge | 8080 | Main API service |

## Access URLs

- **MinIO Console**: http://localhost:9001
  - Username: `minioadmin`
  - Password: `minioadmin`

- **Adminer**: http://localhost:8081
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
docker compose -f docker-compose.dev.yml down

# Stop and remove data
docker compose -f docker-compose.dev.yml down -v

# View logs
docker compose -f docker-compose.dev.yml logs -f keystone-edge-dev

# Execute command in container
docker exec -it keystone-edge-dev sh
```

## Testing

Run the complete test suite:

```bash
# Automated testing with build + test
./scripts/test.sh
```

The test script will:
1. Build the Docker image
2. Start all services (MySQL, MinIO, Keystone)
3. Wait for services to be healthy
4. Run automated tests against the API
5. Collect logs
6. Clean up

Tests include:
- Health check endpoint (`/api/v1/health`)
- Swagger documentation (`/swagger/doc.json`)
- Swagger UI (`/swagger/index.html`)
- Response JSON validation
