// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package cloudpb contains generated gRPC/protobuf bindings for the
// ArcheBase data-platform cloud API.
//
// Proto source files are in the proto/ subdirectory. To regenerate the Go
// bindings after modifying a .proto file, run from the keystone/ directory:
//
//	make proto
//
// or invoke go generate directly:
//
//	go generate ./internal/cloud/cloudpb/
//
// Required tools (install once):
//
//	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
//	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
//
// protoc v5.x must also be available on PATH.
package cloudpb

//go:generate protoc -I proto --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative common.proto auth.proto data_gateway.proto
