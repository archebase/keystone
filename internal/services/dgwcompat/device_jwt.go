// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"archebase.com/keystone-edge/internal/services/deviceauth"
)

type devicePrincipal = deviceauth.Principal

func deviceUnaryAuthInterceptor(authenticator *deviceauth.Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		principal, err := authenticateDeviceContext(ctx, authenticator)
		if err != nil {
			return nil, err
		}
		return handler(deviceauth.WithPrincipal(ctx, principal), req)
	}
}

func unifiedUnaryAuthInterceptor(authenticator *deviceauth.Authenticator) grpc.UnaryServerInterceptor {
	deviceAuth := deviceUnaryAuthInterceptor(authenticator)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		switch {
		case strings.HasPrefix(info.FullMethod, "/archebase.data_gateway.v1.DataGatewayService/"):
			return deviceAuth(ctx, req, info, handler)
		case strings.HasPrefix(info.FullMethod, "/archebase.auth.v1.AuthService/"),
			strings.HasPrefix(info.FullMethod, "/archebase.data_gateway.v1.DeviceInitService/"):
			return handler(ctx, req)
		default:
			return nil, status.Error(codes.PermissionDenied, "gRPC service is not exposed by the compatibility server")
		}
	}
}

func authenticateDeviceContext(ctx context.Context, authenticator *deviceauth.Authenticator) (devicePrincipal, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "device bearer token required")
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	principal, err := authenticator.AuthenticateJWT(ctx, parts[1])
	if err != nil {
		if errors.Is(err, deviceauth.ErrInvalidCredential) {
			return devicePrincipal{}, status.Error(codes.Unauthenticated, "invalid device token")
		}
		return devicePrincipal{}, status.Error(codes.Unavailable, "device authentication unavailable")
	}
	return principal, nil
}

func principalFromContext(ctx context.Context) (devicePrincipal, error) {
	principal, ok := deviceauth.PrincipalFromContext(ctx)
	if !ok {
		return devicePrincipal{}, status.Error(codes.Unauthenticated, "device principal missing")
	}
	return principal, nil
}
