# Docker Development Environment

## Start development environment

```bash
# Start all services
docker compose up -d

# Check service status
docker compose ps

# View logs
docker compose logs -f

# Stop services
docker compose down

# Stop and remove data
docker compose down -v
```

## Services

| Service | Port | Description |
|---------|------|-------------|
| MySQL | 3306 | Database |
| MinIO API | 9000 | Object storage API |
| MinIO Console | 9001 | MinIO management UI |
| Redis | 6379 | Cache (optional) |
| Adminer | 8081 | Database management tool |

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

## Run Keystone Edge locally

```bash
# Load environment variables
export $(cat docker/.env.example | xargs)

# Or use direnv
cp docker/.env.example .env
direnv allow

# Run
go run cmd/keystone-edge/main.go
```

## Database connection

```bash
# Using MySQL client
mysql -h 127.0.0.1 -P 3306 -u keystone -pkeystone keystone

# Using Docker exec
docker exec -it keystone-mysql mysql -ukeystone -pkeystone keystone
```

## MinIO operations

```bash
# Install MinIO client
wget https://dl.min.io/client/mc/release/linux-amd64/mc
chmod +x mc
sudo mv mc /usr/local/bin/

# Configure alias
mc alias set local http://localhost:9000 minioadmin minioadmin

# List buckets
mc ls local

# View bucket contents
mc ls local/edge-factory-shanghai
```
