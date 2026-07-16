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

// Server owns the compatibility gRPC server.
type Server struct {
	cfg     Config
	servers []*grpc.Server
}

type episodeQAEnqueuer interface {
	EnqueueEpisode(episodeID int64)
}

// StartFromEnv starts the compatibility server if KEYSTONE_DGW_COMPAT_ENABLED is truthy.
func StartFromEnv(db *sqlx.DB, qaEnqueuer episodeQAEnqueuer) (*Server, error) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		logger.Println("[DGW_COMPAT] Compatibility server disabled")
		return nil, nil
	}
	return Start(cfg, db, qaEnqueuer)
}

// Start starts the compatibility gRPC listener.
func Start(cfg Config, db *sqlx.DB, qaEnqueuer episodeQAEnqueuer) (*Server, error) {
	if db == nil {
		return nil, fmt.Errorf("dgw compatibility server requires database")
	}
	sts, err := newSTSProvider(cfg)
	if err != nil {
		return nil, err
	}
	sessions := newSessionStore()
	identity := newDeviceIdentityService(db, cfg)

	server := grpc.NewServer(grpc.UnaryInterceptor(unifiedUnaryAuthInterceptor(db, cfg)))
	cloudpb.RegisterAuthServiceServer(server, &authService{identity: identity})
	cloudpb.RegisterDataGatewayServiceServer(server, newGatewayService(cfg, sts, sessions, db, qaEnqueuer))
	cloudpb.RegisterDeviceInitServiceServer(server, &deviceInitService{identity: identity})

	started := &Server{cfg: cfg}
	if err := started.serve("grpc", cfg.GRPCAddr, server); err != nil {
		started.Stop(context.Background())
		return nil, err
	}
	logger.Printf("[DGW_COMPAT] Started compatibility gRPC server addr=%s services=auth,gateway,device_init backend=volcengine_tos bucket=%s endpoint=%s region=%s mock_sts=%t",
		cfg.GRPCAddr, cfg.TOSBucket, cfg.TOSEndpoint, cfg.TOSRegion, cfg.MockSTS)
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
	logger.Println("[DGW_COMPAT] Compatibility gRPC server stopped")
}
