// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"fmt"
	"net"
	"sync"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc"
)

// Server owns the compatibility gRPC servers.
type Server struct {
	cfg     Config
	servers []*grpc.Server
}

// StartFromEnv starts the compatibility server if KEYSTONE_DGW_COMPAT_ENABLED is truthy.
func StartFromEnv(db *sqlx.DB) (*Server, error) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		logger.Println("[DGW_COMPAT] Compatibility server disabled")
		return nil, nil
	}
	return Start(cfg, db)
}

// Start starts all compatibility gRPC listeners.
func Start(cfg Config, db *sqlx.DB) (*Server, error) {
	if db == nil {
		return nil, fmt.Errorf("dgw compatibility server requires database")
	}
	sts, err := newSTSProvider(cfg)
	if err != nil {
		return nil, err
	}
	sessions := newSessionStore()
	identity := newDeviceIdentityService(db, cfg)

	auth := grpc.NewServer()
	cloudpb.RegisterAuthServiceServer(auth, &authService{identity: identity})

	gateway := grpc.NewServer(grpc.UnaryInterceptor(deviceUnaryAuthInterceptor(db, cfg)))
	cloudpb.RegisterDataGatewayServiceServer(gateway, newGatewayService(cfg, sts, sessions))

	deviceInit := grpc.NewServer()
	cloudpb.RegisterDeviceInitServiceServer(deviceInit, &deviceInitService{identity: identity})

	started := &Server{cfg: cfg}
	if err := started.serve("auth", cfg.AuthAddr, auth); err != nil {
		started.Stop(context.Background())
		return nil, err
	}
	if err := started.serve("gateway", cfg.GatewayAddr, gateway); err != nil {
		started.Stop(context.Background())
		return nil, err
	}
	if err := started.serve("device_init", cfg.DeviceInitAddr, deviceInit); err != nil {
		started.Stop(context.Background())
		return nil, err
	}
	logger.Printf("[DGW_COMPAT] Started compatibility gRPC servers auth=%s gateway=%s device_init=%s backend=volcengine_tos bucket=%s endpoint=%s region=%s mock_sts=%t",
		cfg.AuthAddr, cfg.GatewayAddr, cfg.DeviceInitAddr, cfg.TOSBucket, cfg.TOSEndpoint, cfg.TOSRegion, cfg.MockSTS)
	return started, nil
}

func (s *Server) serve(name, addr string, grpcServer *grpc.Server) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", name, addr, err)
	}
	s.servers = append(s.servers, grpcServer)
	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			logger.Printf("[DGW_COMPAT] %s server stopped with error: %v", name, err)
		}
	}()
	return nil
}

// Stop stops all compatibility gRPC servers.
func (s *Server) Stop(ctx context.Context) {
	if s == nil {
		return
	}
	var wg sync.WaitGroup
	for _, grpcServer := range s.servers {
		wg.Add(1)
		go func(server *grpc.Server) {
			defer wg.Done()
			done := make(chan struct{})
			go func() {
				server.GracefulStop()
				close(done)
			}()
			select {
			case <-ctx.Done():
				server.Stop()
			case <-done:
			}
		}(grpcServer)
	}
	wg.Wait()
	logger.Println("[DGW_COMPAT] Compatibility gRPC servers stopped")
}
