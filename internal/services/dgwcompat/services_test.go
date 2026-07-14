// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
)

type fixedSTSProvider struct {
	expiration time.Time
}

func (p fixedSTSProvider) AssumeRole(context.Context, stsScope) (stsCredentials, error) {
	return stsCredentials{
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		SecurityToken:   "token",
		Expiration:      p.expiration,
	}, nil
}

func TestGatewayCreateReissueCompleteFlow(t *testing.T) {
	expiration := time.Unix(2200, 0).UTC()
	cfg := Config{
		TOSBucket:      "bucket-1",
		TOSEndpoint:    "https://tos-cn-beijing.volces.com",
		TOSRegion:      "cn-beijing",
		TOSKeyPrefix:   "device-uploads",
		UploadPartSize: 8 * 1024 * 1024,
	}
	service := newGatewayService(cfg, fixedSTSProvider{expiration: expiration}, newSessionStore())
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		DeviceID: "robot-1", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":  "robot-1",
			"capture_id": "capture-1",
			"task_id":    "task-1",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	if created.GetLogicalUploadId() == "" || created.GetUploadId() == "" {
		t.Fatalf("CreateLogicalUpload() returned empty ids: %+v", created)
	}
	credentials := created.GetCredentials()
	if credentials.GetBucket() != "bucket-1" {
		t.Fatalf("bucket = %q, want bucket-1", credentials.GetBucket())
	}
	if credentials.GetStsSecurityToken() != "token" {
		t.Fatalf("security token = %q, want token", credentials.GetStsSecurityToken())
	}

	reissued, err := service.ReissueUploadCredentials(ctx, &cloudpb.ReissueUploadCredentialsRequest{
		UploadId: created.GetUploadId(),
	})
	if err != nil {
		t.Fatalf("ReissueUploadCredentials() error = %v", err)
	}
	if reissued.GetCredentials().GetObjectKey() != credentials.GetObjectKey() {
		t.Fatalf("reissued object key = %q, want %q", reissued.GetCredentials().GetObjectKey(), credentials.GetObjectKey())
	}

	recovery, err := service.GetUploadRecovery(ctx, &cloudpb.GetUploadRecoveryRequest{
		LogicalUploadId: created.GetLogicalUploadId(),
	})
	if err != nil {
		t.Fatalf("GetUploadRecovery() error = %v", err)
	}
	if recovery.GetCredentialRefreshCount() != 1 {
		t.Fatalf("refresh count = %d, want 1", recovery.GetCredentialRefreshCount())
	}

	_, err = service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"capture_id": "capture-1",
			"task_id":    "task-1",
			"device_id":  "robot-1",
		},
	})
	if err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
}
