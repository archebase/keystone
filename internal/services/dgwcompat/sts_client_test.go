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

func TestTOSUploadPolicyScopesOneExactObjectPlusMultipartList(t *testing.T) {
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
	const exact = "trn:tos:::bucket-a/uploads/device-1/file.mcap"

	// Plain object + single-object write/head operations stay scoped to the
	// exact object key (no wildcard, no whole-bucket fallback).
	write := findStatementWithAction(decoded.Statement, "tos:PutObject")
	if write == nil {
		t.Fatalf("missing write statement: %s", policy)
	}
	if !equalStrings(write.Resource, []string{exact}) {
		t.Fatalf("write resource=%v want=[%s]", write.Resource, exact)
	}

	// Resume flow listing uploaded parts must be allowed on the object and its
	// multipart sub-resource, under both TOS list action names.
	list := findStatementWithAction(decoded.Statement, "tos:ListParts")
	if list == nil {
		t.Fatalf("missing multipart-list statement: %s", policy)
	}
	for _, wantAction := range []string{"tos:ListParts", "tos:ListMultipartUploadParts"} {
		if !containsString(list.Action, wantAction) {
			t.Fatalf("list statement missing action %s: %s", wantAction, policy)
		}
	}
	if !equalStrings(list.Resource, []string{exact, exact + "/*"}) {
		t.Fatalf("list resource=%v want=[%s %s]", list.Resource, exact, exact+"/*")
	}
	// Never fall back to a whole-bucket scope.
	if strings.Contains(policy, `trn:tos:::bucket-a"`) {
		t.Fatalf("policy contains whole-bucket resource: %s", policy)
	}
}

func findStatementWithAction(statements []struct {
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}, action string,
) *struct {
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
} {
	for i := range statements {
		if containsString(statements[i].Action, action) {
			return &statements[i]
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
