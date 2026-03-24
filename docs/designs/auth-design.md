<!--
SPDX-FileCopyrightText: 2026 ArcheBase

SPDX-License-Identifier: MulanPSL-2.0
-->

# Authentication & Authorization Design Document

**Status:** Planned | **Version:** 0.1.0

## 1. Overview

Keystone Edge requires authentication and authorization for its APIs. This document describes how to implement JWT-based authentication.

### 1.1 Design Goals

- **Simple Implementation**: Each role uses separate tables
- **JWT Authentication**: For Synapse UI user login
- **Data Isolation**: Implement data scope control based on role

### 1.2 Supported Roles

| Role | Table | Description |
|------|-------|-------------|
| `data_collector` | `data_collectors` | Data collector, works on Synapse UI |

> **Future Extensions**: `admin`, `inspector` and other roles can be added with separate tables, maintaining consistent architecture.

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Client Layer                                   │
│                                                                             │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                         Synapse UI (Browser)                          │  │
│  └───────────────────────────────┬───────────────────────────────────────┘  │
│                                  │                                          │
│                                  ▼                                          │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                       Authorization Header                            │  │
│  │                       Bearer <jwt_token>                              │  │
│  └───────────────────────────────┬───────────────────────────────────────┘  │
└──────────────────────────────────┼──────────────────────────────────────────┘
                                   ▼
    ┌───────────────────────────────────────────────────────────────┐
    │                       Auth Middleware                         │
    │  ┌──────────────┐  ┌───────────────┐  ┌────────────────────┐  │
    │  │  JWT Parser  │→ │Token Lookup  │→ │   Scope Check       │  │
    │  └──────────────┘  └───────────────┘  └────────────────────┘  │
    └────────────────────────────┬──────────────────────────────────┘
                                 ▼
    ┌───────────────────────────────────────────────────────────────┐
    │                      Protected Routes                         │
    │  ┌─────────────────────────────────────────────────────┐      │
    │  │              /api/v1/*                              │      │
    │  │                                                     │      │
    │  │  All authenticated API endpoints                    │      │
    │  └─────────────────────────────────────────────────────┘      │
    └───────────────────────────────────────────────────────────────┘
```

### Authentication Method

| Client Type | Header | Token Type | Description |
|------------|--------|------------|-------------|
| Synapse UI (Browser) | `Authorization: Bearer <jwt>` | JWT | Login via username+password, 24-hour expiry |

---

## 3. Database Schema

### 3.1 Migration File

Create `internal/storage/database/migrations/000002_add_auth.up.sql`:

```sql
-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Add authentication fields to data_collectors table

ALTER TABLE data_collectors
    ADD COLUMN password_hash VARCHAR(255) NULL COMMENT 'Bcrypt hash for password login' AFTER email,
    ADD COLUMN last_login_at TIMESTAMP NULL COMMENT 'Last successful login time' AFTER password_hash;
```

### 3.2 Down Migration

Create `internal/storage/database/migrations/000002_add_auth.down.sql`:

```sql
-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE data_collectors
    DROP COLUMN password_hash,
    DROP COLUMN last_login_at;
```

---

## 4. Configuration

### 4.1 Config Structure

Add to [`internal/config/config.go`](internal/config/config.go:26):

```go
// AuthConfig authentication and authorization configuration
type AuthConfig struct {
    JWTSecret      string // JWT signing secret (min 32 chars)
    JWTTokenExpiry int    // Token expiry in hours (default: 24)
    Issuer         string // Token issuer (default: "keystone-edge")
}
```

### 4.2 Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `KEYSTONE_JWT_SECRET` | JWT signing secret | (required) |
| `KEYSTONE_JWT_EXPIRY_HOURS` | Token expiry in hours | 24 |
| `KEYSTONE_JWT_ISSUER` | Token issuer | keystone-edge |

---

## 5. JWT Token

### 5.1 JWT Token (for UI)

**Flow**: `username + password` → `POST /auth/login` → `JWT`

JWT contains the following claims:

```json
// With workstation assigned
{
  "sub_id": 123,
  "role": "data_collector",
  "identifier": "DC-001",
  "workstation_id": 5,
  "factory_id": 1,
  "iss": "keystone-edge",
  "exp": 1700000000,
  "iat": 1699990000
}

// Without workstation assigned (workstation_id not present)
{
  "sub_id": 123,
  "role": "data_collector",
  "identifier": "DC-001",
  "factory_id": 1,
  "iss": "keystone-edge",
  "exp": 1700000000,
  "iat": 1699990000
}
```

**Usage**:
```bash
curl -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIs..." https://api.example.com/api/v1/...
```

---

## 6. Code Structure

```
internal/
├── auth/
│   ├── claims.go          # JWT claims structure
│   └── jwt.go             # JWT generation and validation
└── middleware/
    └── auth.go            # Gin authentication middleware

internal/api/handlers/
├── auth.go                 # Authentication handlers (login, logout, profile)
└── auth_test.go           # Tests
```

### 6.1 Claims Structure

[`internal/auth/claims.go`](internal/auth/claims.go):

```go
package auth

import (
    "github.com/golang-jwt/jwt/v5"
)

// Claims represents JWT claims for authentication
type Claims struct {
    SubjectID     int64   `json:"sub_id"`                     // User ID
    Role          string  `json:"role"`                       // Role name: "data_collector"
    Identifier    string  `json:"identifier"`                  // Login identifier (operator_id)
    WorkstationID *int64 `json:"workstation_id,omitempty"`   // Primary workstation (optional)
    FactoryID     int64   `json:"factory_id"`                 // Factory scope
    jwt.RegisteredClaims
}

// NewDataCollectorClaims creates claims for a data collector
// workstationID can be nil (when DC is not assigned to a workstation yet)
func NewDataCollectorClaims(id int64, operatorID string, workstationID *int64, factoryID int64) *Claims {
    return &Claims{
        SubjectID:     id,
        Role:          "data_collector",
        Identifier:    operatorID,
        WorkstationID: workstationID,  // May be nil
        FactoryID:     factoryID,
    }
}
```

### 6.2 JWT Utilities

[`internal/auth/jwt.go`](internal/auth/jwt.go):

```go
package auth

import (
    "errors"
    "time"

    "github.com/golang-jwt/jwt/v5"
    "archebase.com/keystone-edge/internal/config"
)

var (
    ErrInvalidToken = errors.New("invalid token")
    ErrExpiredToken = errors.New("token has expired")
)

// GenerateToken creates a new JWT token
func GenerateToken(claims *Claims, cfg *config.AuthConfig) (string, error) {
    claims.Issuer = cfg.Issuer
    claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Duration(cfg.JWTTokenExpiry) * time.Hour))
    claims.IssuedAt = jwt.NewNumericDate(time.Now())
    claims.NotBefore = jwt.NewNumericDate(time.Now())

    token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
    return token.SignedString([]byte(cfg.JWTSecret))
}

// ParseToken validates and parses a JWT token
func ParseToken(tokenString string, cfg *config.AuthConfig) (*Claims, error) {
    token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
        if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
            return nil, ErrInvalidToken
        }
        return []byte(cfg.JWTSecret), nil
    })

    if err != nil {
        if errors.Is(err, jwt.ErrTokenExpired) {
            return nil, ErrExpiredToken
        }
        return nil, ErrInvalidToken
    }

    if claims, ok := token.Claims.(*Claims); ok && token.Valid {
        return claims, nil
    }
    return nil, ErrInvalidToken
}
```

### 6.3 Authentication Middleware

[`internal/middleware/auth.go`](internal/middleware/auth.go):

```go
package middleware

import (
    "net/http"
    "strings"

    "github.com/gin-gonic/gin"
    "archebase.com/keystone-edge/internal/auth"
    "archebase.com/keystone-edge/internal/config"
)

const ClaimsKey = "auth_claims"

// JWTAuth middleware validates JWT tokens
// Header: Authorization: Bearer <jwt_token>
func JWTAuth(cfg *config.AuthConfig) gin.HandlerFunc {
    return func(c *gin.Context) {
        authHeader := c.GetHeader("Authorization")
        if authHeader == "" {
            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
            return
        }

        // Expect "Bearer <token>"
        parts := strings.SplitN(authHeader, " ", 2)
        if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization format"})
            return
        }

        claims, err := auth.ParseToken(parts[1], cfg)
        if err != nil {
            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
            return
        }

        c.Set(ClaimsKey, claims)
        c.Next()
    }
}

// GetClaims extracts claims from context
func GetClaims(c *gin.Context) *auth.Claims {
    if v, exists := c.Get(ClaimsKey); exists {
        return v.(*auth.Claims)
    }
    return nil
}

// RequireRole middleware checks the role
func RequireRole(allowedRoles ...string) gin.HandlerFunc {
    return func(c *gin.Context) {
        claims := GetClaims(c)
        if claims == nil {
            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
            return
        }

        for _, role := range allowedRoles {
            if claims.Role == role {
                c.Next()
                return
            }
        }

        c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
    }
}
```

---

## 7. HTTP API

### 7.1 Authentication Endpoints

Routes mounted under `/auth`:

| Method | Path | Description | Auth |
|--------|------|-------------|------|
| POST | `/auth/login` | Unified login (all roles) | None |
| POST | `/auth/logout` | Logout | JWT |
| POST | `/auth/refresh` | Refresh token | JWT |
| GET | `/auth/profile` | Get current user info | JWT |

### 7.2 Login API

**POST** `/auth/login`

Unified login endpoint for all roles:

Request body:
```json
{
  "username": "DC-001",
  "password": "secret123"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| username | string | Yes | Login identifier (operator_id for data_collector) |
| password | string | Yes | Password (plaintext) |

Success response (200):
```json
// With workstation assigned
{
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "expires_in": 86400,
  "user": {
    "id": 1,
    "role": "data_collector",
    "identifier": "DC-001",
    "name": "Zhang San",
    "workstation_id": 5
  }
}

// Without workstation assigned
{
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "expires_in": 86400,
  "user": {
    "id": 1,
    "role": "data_collector",
    "identifier": "DC-001",
    "name": "Zhang San"
    // workstation_id not present
  }
}
```

Error response (401):
```json
{
  "error": "invalid credentials"
}
```

**Login Flow**:
```
POST /auth/login
    │
    ▼
Look up user in corresponding role table (data_collectors)
    │
    ▼
Verify password_hash (bcrypt)
    │
    ▼
Query workstations table for workstation_id (may be null)
    │
    ▼
Generate JWT with role, workstation_id, etc.
```

### 7.3 Create Data Collector API

**POST** `/api/v1/data_collectors`

Password is required when creating a data collector:

Request body:
```json
{
  "name": "Zhang San",
  "operator_id": "DC-001",
  "password": "initial_password123"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Data collector name |
| operator_id | string | Yes | Unique operator identifier |
| password | string | Yes | Initial password (will be bcrypt hashed) |

> **Note**: Creating data collectors requires admin privileges. Can be protected with admin role middleware in future extensions.

### 7.4 Protected Routes

All authenticated API endpoints require JWT in the request header:

```
Authorization: Bearer <jwt_token>
```

---

## 8. Implementation Plan

### Phase 1: Core Infrastructure

| Task | File | Priority |
|------|------|----------|
| Add AuthConfig to config | `internal/config/config.go` | High |
| Create auth package | `internal/auth/claims.go` | High |
| JWT utilities | `internal/auth/jwt.go` | High |
| Database migration | `migrations/000002_add_auth.up.sql` | High |

### Phase 2: Middleware & Handlers

| Task | File | Priority |
|------|------|----------|
| Auth middleware | `internal/middleware/auth.go` | High |
| Auth handlers | `internal/api/handlers/auth.go` | High |
| Apply middleware to routes | `internal/server/server.go` | High |

### Phase 3: Future Extensions

| Task | Description |
|------|-------------|
| Additional roles | Admin, Inspector roles (separate tables) |
| Password reset | Password reset functionality |
| MFA | Two-factor authentication |

---

## 9. Security Considerations

### 9.1 Token Storage

- JWT secret must be at least 32 characters
- Store JWT secret in environment variable, never in code
- Implement token blacklisting for immediate logout

### 9.2 Data Isolation

Data collector can only access:
- Their own profile
- Workstations they are assigned to (via `workstations.data_collector_id`)
- Tasks assigned to those workstations
- Episodes from those tasks

### 9.3 Future Enhancements

| Enhancement | Description |
|-------------|-------------|
| Token blacklisting | Redis-based for immediate logout |
| Rate limiting | Per-IP and per-user limits |
| Audit logging | Log all authentication events |

---

## 10. Testing

### 10.1 Unit Tests

```go
// internal/auth/jwt_test.go
func TestGenerateAndParseToken(t *testing.T) {
    cfg := &config.AuthConfig{
        JWTSecret:      "test-secret-key-must-be-32-chars",
        JWTTokenExpiry: 24,
        Issuer:         "test",
    }

    var wsID int64 = 5
    claims := auth.NewDataCollectorClaims(1, "DC-001", &wsID, 1)
    token, err := auth.GenerateToken(claims, cfg)
    require.NoError(t, err)

    parsed, err := auth.ParseToken(token, cfg)
    require.NoError(t, err)
    assert.Equal(t, int64(1), parsed.SubjectID)
    assert.Equal(t, "data_collector", parsed.Role)
}
```

### 10.2 Integration Tests

```bash
# Test login flow
curl -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"DC-001","password":"secret"}'

# Test protected endpoint
curl http://localhost:8080/api/v1/... \
  -H "Authorization: Bearer <token>"
```
