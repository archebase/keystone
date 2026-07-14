// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
)

type fakeVolcengineSTSClient struct {
	input  *sts.AssumeRoleInput
	output *sts.AssumeRoleOutput
	err    error
}

func (c *fakeVolcengineSTSClient) AssumeRoleWithContext(
	_ volcengine.Context,
	input *sts.AssumeRoleInput,
	_ ...request.Option,
) (*sts.AssumeRoleOutput, error) {
	c.input = input
	return c.output, c.err
}

func TestTOSUploadPolicyScopesOneExactObject(t *testing.T) {
	policy, err := tosUploadPolicy(stsScope{Bucket: "bucket-a", ObjectKey: "uploads/device-1/file.mcap"})
	if err != nil {
		t.Fatalf("tosUploadPolicy() error = %v", err)
	}
	var decoded struct {
		Statement []struct {
			Action   []string `json:"Action"`
			Resource []string `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &decoded); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if len(decoded.Statement) != 1 || len(decoded.Statement[0].Resource) != 1 {
		t.Fatalf("unexpected policy shape: %s", policy)
	}
	const want = "trn:tos:::bucket-a/uploads/device-1/file.mcap"
	if decoded.Statement[0].Resource[0] != want {
		t.Fatalf("resource=%q want=%q", decoded.Statement[0].Resource[0], want)
	}
	if strings.Contains(policy, "bucket-a/*") || strings.Contains(policy, want+"*") {
		t.Fatalf("policy contains wildcard scope: %s", policy)
	}
}

func TestVolcengineSTSProviderParsesCredentialsAndRequest(t *testing.T) {
	client := &fakeVolcengineSTSClient{output: (&sts.AssumeRoleOutput{}).SetCredentials(
		(&sts.CredentialsForAssumeRoleOutput{}).
			SetAccessKeyId(" temp-ak ").
			SetSecretAccessKey(" temp-sk ").
			SetSessionToken(" temp-token ").
			SetExpiredTime("2026-07-14T12:30:00Z"),
	)}
	provider := &volcengineSTSProvider{client: client, roleTRN: "trn:iam::123:role/test", sessionTTL: 12 * time.Minute}

	credentials, err := provider.AssumeRole(context.Background(), stsScope{Bucket: "bucket-a", ObjectKey: "one.mcap"})
	if err != nil {
		t.Fatalf("AssumeRole() error = %v", err)
	}
	if credentials.AccessKeyID != "temp-ak" || credentials.AccessKeySecret != "temp-sk" || credentials.SecurityToken != "temp-token" {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
	if credentials.Expiration != time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC) {
		t.Fatalf("expiration=%s", credentials.Expiration)
	}
	if client.input == nil || client.input.Policy == nil || !strings.Contains(*client.input.Policy, "trn:tos:::bucket-a/one.mcap") {
		t.Fatalf("unexpected AssumeRole input: %#v", client.input)
	}
	if client.input.DurationSeconds == nil || *client.input.DurationSeconds != 720 {
		t.Fatalf("duration=%v want=720", client.input.DurationSeconds)
	}
}

func TestVolcengineSTSProviderDoesNotExposeProviderErrorText(t *testing.T) {
	client := &fakeVolcengineSTSClient{err: errors.New("request contained long-ak long-sk long-token")}
	provider := &volcengineSTSProvider{client: client, roleTRN: "role", sessionTTL: time.Minute}

	_, err := provider.AssumeRole(context.Background(), stsScope{Bucket: "bucket", ObjectKey: "object"})
	if err == nil {
		t.Fatal("AssumeRole() error = nil")
	}
	for _, secret := range []string{"long-ak", "long-sk", "long-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
}
