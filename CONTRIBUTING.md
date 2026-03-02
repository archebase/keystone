# Contributing to Keystone

Thank you for your interest in contributing to Keystone! This document provides essential information for all contributors.

---

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Contributing Guidelines](#contributing-guidelines)
- [Development Workflow](#development-workflow)
- [Code Style](#code-style)
- [Testing](#testing)
- [Building](#building)
- [Submitting Changes](#submitting-changes)
- [Reporting Issues](#reporting-issues)
- [Community Guidelines](#community-guidelines)

---

## Code of Conduct

**See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)** for the full Code of Conduct.

---

## Getting Started

### Ways to Contribute

There are many ways to contribute to Keystone:

1. **Report bugs** - Submit bug reports to help us improve
2. **Suggest features** - Request features that would be useful to you
3. **Write code** - Fix bugs or implement features
4. **Improve documentation** - Help make Keystone easier to understand
5. **Review pull requests** - Help review and test contributions
6. **Answer questions** - Help other users on GitHub Discussions
7. **Share your use case** - Tell us how you're using Keystone

### First-Time Contributors

We welcome first-time contributors! Start with:

- Issues labeled `good first issue`
- Issues labeled `help wanted`
- Documentation improvements
- Bug fixes

---

## Development Setup

### Prerequisites

**System Dependencies:**

- Go 1.24+
- Docker & Docker Compose (for development)

**Development Tools:**

- [swag](https://github.com/swaggo/swag) - For generating Swagger/OpenAPI documentation
- [golangci-lint](https://github.com/golangci/golangci-lint) - Code quality checking
- [gofmt](https://pkg.go.dev/cmd/gofmt) - Code formatting (built into Go)

### Initial Setup

1. **Fork and clone the repository:**

```bash
# Fork the repository on GitHub first
git clone https://github.com/YOUR_USERNAME/keystone.git
cd keystone
git remote add upstream https://github.com/ArcheBase/keystone.git
```

2. **Copy environment configuration:**

```bash
cp docker/.env.example .env
```

3. **Start development services:**

```bash
# Start dependency services (MySQL, MinIO, Redis) using Docker Compose
docker compose -f docker/docker-compose.dev.yml up -d

# Or run the development script
./run-dev.sh
```

4. **Verify installation:**

```bash
# Check if services are running properly
curl http://localhost:8080/api/v1/health

# Access Swagger documentation
# http://localhost:8080/swagger/index.html
```

---

## Contributing Guidelines

### License

By contributing to Keystone, you agree that your contributions will be licensed under the **MIT** License.

All source files must include the following license header:

```go
// Copyright (c) 2026 ArcheBase
// Keystone is licensed under MIT License.
// You can use this software according to the terms and conditions of the MIT License.
// You may obtain a copy of MIT License at:
//          https://opensource.org/licenses/MIT
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the MIT License for more details.
```

### We Welcome

- Bug fixes
- New features (please discuss in an issue first)
- Performance improvements
- Documentation improvements
- Test additions
- Code refactoring (maintaining functionality)

### We Generally Do Not Accept

- Undiscussed breaking changes
- Features that don't align with project goals
- Large refactors without prior approval
- Changes that reduce code coverage
- Dependencies on proprietary software

### Design Philosophy

Keystone follows these design principles:

1. **Go-first** - Pure Go implementation, no Python dependencies
2. **Edge-ready** - Designed for edge data collection scenarios
3. **State-machine driven** - Task lifecycle management
4. **Quality assurance** - Built-in QA engine
5. **Cloud sync** - Support for cloud synchronization

Please ensure your contributions align with these principles.

---

## Development Workflow

### Branching Strategy

- **`main`** - Protected branch, all PRs must target this
- **Feature branches** - Named like `feature/your-feature` or `fix/your-bug-fix`
- **Release branches** - Named like `release/vX.Y.Z`

### Creating a Feature Branch

```bash
git checkout main
git fetch upstream
git rebase upstream/main
git checkout -b feature/your-feature-name
```

### Commit Message Format

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

**Types:**

- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `style`: Code style changes (formatting, etc.)
- `refactor`: Code refactoring
- `perf`: Performance improvement
- `test`: Adding or updating tests
- `chore`: Maintenance tasks
- `ci`: CI/CD changes

**Scopes:**

- `api`: HTTP API handlers
- `config`: Configuration related
- `server`: HTTP server
- `storage`: Storage layer (MySQL, MinIO, Queue)
- `services`: Business service layer
- `models`: Data models
- `test`: Test code
- `docs`: Documentation
- `build`: Build system

**Examples:**

```
feat(api): add task creation endpoint

Implement a new endpoint for creating recording tasks.
This includes validation, state machine initialization,
and database persistence.

Closes #123
```

```
fix(storage): resolve MySQL connection leak

The database connection was not being properly closed
in the error path, causing connection pool exhaustion.
This fix ensures proper resource cleanup.

Fixes #145
```

### Making Changes

1. Write code following our [Code Style](#code-style)
2. Add tests for new features
3. Run tests locally
4. Format your code
5. Commit with conventional commit messages
6. Push to your fork
7. Create a pull request

---

## Code Style

### Code Formatting

Keystone uses Go's standard tools for code formatting:

```bash
# Format all files
go fmt ./...

# Or use gofmt for more options
gofmt -w .
```

**CI Checks:**

```bash
# Check formatting (without modifying files)
gofmt -l ./

# If files are not formatted, CI will fail
```

### Go Best Practices

**General:**

- Use idiomatic Go
- Always use `errcheck` to check errors
- Use `context` for timeouts and cancellation
- Follow Go code conventions
- Use `const` and `iota` for constants

**Error Handling:**

- Always check errors, never ignore them
- Use meaningful error messages
- Wrap errors to preserve stack trace
- Return meaningful errors to callers

**Concurrency Safety:**

- Document thread safety guarantees
- Use `sync.Mutex` or `sync.RWMutex`
- Use `channels` for communication
- Avoid shared memory, prefer communication

**Performance:**

- Avoid premature optimization
- Profile before optimizing
- Use appropriate data structures
- Minimize allocations in hot paths
- Consider cache locality

### Documentation

- All exported functions and types must have documentation comments
- Use complete sentences
- Include usage examples (for complex APIs)
- Keep comments in sync with code changes

Example:

```go
// TaskManager is responsible for managing the complete lifecycle of tasks.
// It handles task creation, state transitions, interaction with the storage layer,
// and callbacks with external systems (such as the Axon recorder).
type TaskManager struct {
    // store is the task persistence storage
    store *storage.TaskStore
    // queue is the message queue for async processing
    queue *queue.Queue
}

// CreateTask creates a new task and initializes its state machine.
// It persists the task to the database and adds it to the processing queue.
//
// Parameters:
//   - ctx: Context for timeout and cancellation
//   - req: Request parameters for creating the task
//
// Returns:
//   - The created task object
//   - Error information (if any)
func (tm *TaskManager) CreateTask(ctx context.Context, req *CreateTaskRequest) (*Task, error) {
    // Implementation code
}
```

---

## Testing

### Testing Philosophy

- **Test-Driven Development**: Write tests before or alongside code
- **Isolation**: Tests should be independent and runnable in any order
- **Speed**: Unit tests should be fast (each < 0.1s)
- **Clarity**: Tests should serve as documentation

### Running Tests Locally

**Unit Tests:**

```bash
# Run all unit tests
go test -v ./...

# With coverage
go test -cover -race -v ./...
```

**Integration Tests:**

```bash
# Run integration tests (requires Docker)
bash scripts/test.sh

# Or manually start services then test
docker compose -f docker/docker-compose.test.yml up -d
go test -tags=integration -v ./...
```

**Specific Module Tests:**

```bash
# Test specific package
go test -v ./internal/config/...

# Test specific package with verbose output
go test -v -run TestConfigLoad ./internal/config/

# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

### Test Structure

```
keystone/
├── cmd/
│   └── keystone-edge/
│       └── main.go
├── internal/
│   ├── api/
│   │   └── handlers/
│   │       └── handler_test.go
│   ├── config/
│   │   └── config_test.go
│   ├── services/
│   │   └── task_manager_test.go
│   └── storage/
│       ├── database/
│       ├── queue/
│       └── s3/
└── pkg/
    ├── models/
    │   └── models_test.go
    └── util/
        └── util_test.go
```

### Writing Tests

**Unit Tests:**

```go
package config_test

import (
    "testing"

    "archebase.com/keystone-edge/internal/config"
)

func TestLoadConfig(t *testing.T) {
    // Set test environment variables
    t.Setenv("KEYSTONE_BIND_ADDR", ":9090")
    t.Setenv("KEYSTONE_MYSQL_HOST", "localhost")

    // Load configuration
    cfg, err := config.Load()

    // Verify results
    if err != nil {
        t.Fatalf("expected no error, got %v", err)
    }
    if cfg.BindAddr != ":9090" {
        t.Errorf("expected bind addr :9090, got %s", cfg.BindAddr)
    }
}

func TestConfigValidation(t *testing.T) {
    tests := []struct {
        name    string
        cfg     *config.Config
        wantErr bool
    }{
        {
            name: "valid config",
            cfg: &config.Config{
                BindAddr: ":8080",
                MySQL: config.MySQLConfig{
                    Host:     "localhost",
                    Port:     3306,
                    User:     "root",
                    Password: "password",
                    Database: "keystone",
                },
            },
            wantErr: false,
        },
        {
            name: "missing mysql host",
            cfg: &config.Config{
                BindAddr: ":8080",
                MySQL: config.MySQLConfig{
                    Port:     3306,
                    User:     "root",
                    Password: "password",
                    Database: "keystone",
                },
            },
            wantErr: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := tt.cfg.Validate()
            if (err != nil) != tt.wantErr {
                t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

**Test Naming:**

- Use `Test<FunctionName>` format
- Use `Test<FunctionName>/<Scenario>` for subtests
- Clearly describe test scenarios and expected results

**Test Doubles:**

For tests that depend on external services, use interfaces and mocks:

```go
// Define interface
type TaskStore interface {
    Create(ctx context.Context, task *Task) error
    GetByID(ctx context.Context, id string) (*Task, error)
    Update(ctx context.Context, task *Task) error
}

// Use mock in tests
type mockTaskStore struct {
    tasks map[string]*Task
    err   error
}

func (m *mockTaskStore) Create(ctx context.Context, task *Task) error {
    if m.err != nil {
        return m.err
    }
    m.tasks[task.ID] = task
    return nil
}
```

---

## Building

### Build Binary

```bash
# Build binary
go build -o bin/keystone-edge ./cmd/keystone-edge

# Build with version info
go build -ldflags="-s -w" -o bin/keystone-edge ./cmd/keystone-edge
```

### Docker Build

```bash
# Build Docker image
docker build -t keystone-edge:latest .

# Start with Docker Compose
docker compose -f docker/docker-compose.yml up -d
```

### Code Linting

```bash
# Run linter
golangci-lint run

# With auto-fix
golangci-lint run --fix

# Check specific files
golangci-lint run ./internal/config/...
```

---

## Submitting Changes

### Creating a Pull Request

1. Ensure all tests pass
2. Ensure code is formatted
3. Ensure linter has no warnings
4. Update Swagger documentation (if API changes)

```bash
# Generate Swagger documentation
go install github.com/swaggo/swag/cmd/swag@latest
swag init -g internal/server/server.go -o docs
```

5. Commit changes and push to your fork
6. Create a Pull Request on GitHub

### Pull Request Template

Please use the template in [PULL_REQUEST_TEMPLATE.md](../.github/PULL_REQUEST_TEMPLATE.md).

Ensure you include:

- Clear summary of changes
- Motivation and background for changes
- Test results
- Related issue links

### Code Review

- Respond to review comments
- Keep changes small and focused
- Explain complex changes
- Ensure all checks pass

---

## Reporting Issues

### Use Issue Templates

Please use one of the following templates:

- [Bug Report](../.github/ISSUE_TEMPLATE/bug_report.yml) - Report bugs
- [Feature Request](../.github/ISSUE_TEMPLATE/feature_request.yml) - Request new features
- [Documentation Request](../.github/ISSUE_TEMPLATE/documentation_request.yml) - Documentation improvements

### Issue Reporting Best Practices

- Search existing issues to avoid duplicates
- Provide clear title and description
- Include reproduction steps
- Provide environment information (Go version, OS, etc.)
- If possible, provide error logs or screenshots

---

## Community Guidelines

### Getting Help

- GitHub Discussions - Ask questions and share ideas
- GitHub Issues - Report issues and feature requests

### Participating in Discussions

- Respect others
- Stay constructive
- Welcome new members
- Help answer questions

### Continuous Improvement

We're always looking for ways to improve:

- Submit documentation improvements
- Report issues
- Contribute code
- Share usage experience

Thank you for contributing to Keystone!
