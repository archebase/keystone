// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"net"
	"testing"
	"time"

	pb "archebase.com/keystone-edge/internal/cloud/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type completeUploadCaptureServer struct {
	pb.UnimplementedDataGatewayServiceServer
	req *pb.CompleteUploadRequest
}

func (s *completeUploadCaptureServer) CompleteUpload(_ context.Context, req *pb.CompleteUploadRequest) (*pb.CompleteUploadResponse, error) {
	s.req = req
	return &pb.CompleteUploadResponse{}, nil
}

func TestGatewayClientCompleteUploadSendsPartSizeBytes(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	capture := &completeUploadCaptureServer{}
	pb.RegisterDataGatewayServiceServer(server, capture)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("bufconn server exited: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "bufnet", //nolint:staticcheck // bufconn tests still use DialContext.
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	authClient := &AuthClient{
		token: &AuthToken{
			AccessToken: "test-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	client := &GatewayClient{
		cfg: GatewayClientConfig{
			RequestTimeout: time.Second,
		},
		authClient: authClient,
		conn:       conn,
	}

	if err := client.CompleteUpload(ctx, "upload-1", 1234, map[string]string{"k": "v"}, 2, `"etag"`, 8*1024*1024); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	if capture.req == nil {
		t.Fatal("CompleteUpload request was not captured")
	}
	if capture.req.PartSizeBytes != 8*1024*1024 {
		t.Fatalf("PartSizeBytes=%d want %d", capture.req.PartSizeBytes, 8*1024*1024)
	}
	if capture.req.RawTags["k"] != "v" {
		t.Fatalf("RawTags=%+v", capture.req.RawTags)
	}
}
