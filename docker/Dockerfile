# Build stage
FROM golang:1.24-alpine AS builder

# Install build dependencies and swag
RUN apk add --no-cache git && \
    go install github.com/swaggo/swag/cmd/swag@latest

# Add go bin to PATH
ENV PATH="/root/go/bin:${PATH}"

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Generate swagger docs
RUN swag init --parseDependency --parseInternal -g cmd/keystone-edge/main.go

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o /build/keystone-edge \
    ./cmd/keystone-edge

# Final stage
FROM alpine:latest

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates tzdata

# Set timezone
ENV TZ=Asia/Shanghai

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/keystone-edge /app/keystone-edge

# Create non-root user
RUN addgroup -g 1000 keystone && \
    adduser -D -u 1000 -G keystone -h /app keystone && \
    chown -R keystone:keystone /app

USER keystone

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/api/v1/health || exit 1

# Run the binary
CMD ["/app/keystone-edge"]
