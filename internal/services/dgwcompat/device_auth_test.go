// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"errors"
	"testing"
	"time"

	keystoneauth "archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/services"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"
)

type fakeHilbertDeviceClient struct {
	apiKey        string
	validate      bool
	getErr        error
	generateErr   error
	deleteErr     error
	deleteCalls   int
	generateCalls int
}

func (f *fakeHilbertDeviceClient) Login(context.Context, string, string) (*keystoneauth.HilbertLoginResult, error) {
	return keystoneauth.NewHilbertLoginResult(keystoneauth.HilbertAccount{Code: "svc"}, "session"), nil
}

func (f *fakeHilbertDeviceClient) GetDCDeviceAPIKey(context.Context, string, int64, int64) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.apiKey, nil
}

func (f *fakeHilbertDeviceClient) GenerateDCDeviceAPIKey(context.Context, string, int64, int64) (string, error) {
	f.generateCalls++
	if f.generateErr != nil {
		return "", f.generateErr
	}
	return f.apiKey, nil
}

func (f *fakeHilbertDeviceClient) DeleteDCDeviceAPIKey(context.Context, string, int64, int64) error {
	f.deleteCalls++
	return f.deleteErr
}

func (f *fakeHilbertDeviceClient) ValidateDCDeviceAPIKey(context.Context, string, int64, int64, string) (bool, error) {
	return f.validate, nil
}

func TestDeviceInitConsumesSDKPermissionOnce(t *testing.T) {
	db := newDeviceAuthTestDB(t)
	token := seedDeviceAuthToken(t, db, false)
	identity := testDeviceIdentity(db, &fakeHilbertDeviceClient{apiKey: "device-api-key"})
	service := &deviceInitService{identity: identity}
	req := &cloudpb.InitDeviceRequest{DeviceId: "101", DeviceAuthToken: token, Platform: "ios"}

	response, err := service.InitDevice(context.Background(), req)
	if err != nil {
		t.Fatalf("InitDevice() error = %v", err)
	}
	if response.GetApiKey() != "device-api-key" || response.GetTags()["workspace_id"] != "10" {
		t.Fatalf("response = %#v", response)
	}
	_, err = service.InitDevice(context.Background(), req)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("second InitDevice() code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestExchangeCredentialIssuesEpochBoundJWT(t *testing.T) {
	db := newDeviceAuthTestDB(t)
	identity := testDeviceIdentity(db, &fakeHilbertDeviceClient{validate: true})
	service := &authService{identity: identity}

	response, err := service.ExchangeCredential(context.Background(), &cloudpb.ExchangeCredentialRequest{
		DeviceId: "101", Credential: "device-api-key",
	})
	if err != nil {
		t.Fatalf("ExchangeCredential() error = %v", err)
	}
	claims, err := parseDeviceJWT(identity.cfg, response.GetAccessToken())
	if err != nil {
		t.Fatalf("parseDeviceJWT() error = %v", err)
	}
	if claims.DeviceID != "101" || claims.WorkspaceID != 10 || claims.AuthEpoch != 1 {
		t.Fatalf("claims = %#v", claims)
	}
	incoming := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+response.GetAccessToken()))
	if _, err := authenticateDeviceContext(incoming, db, identity.cfg); err != nil {
		t.Fatalf("authenticateDeviceContext() error = %v", err)
	}
	if _, err := db.Exec(`UPDATE robots SET auth_epoch = 2 WHERE id = 1`); err != nil {
		t.Fatalf("increment epoch: %v", err)
	}
	if _, err := authenticateDeviceContext(incoming, db, identity.cfg); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stale token code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestExchangeCredentialRejectsInvalidAPIKey(t *testing.T) {
	db := newDeviceAuthTestDB(t)
	identity := testDeviceIdentity(db, &fakeHilbertDeviceClient{validate: false})
	service := &authService{identity: identity}
	_, err := service.ExchangeCredential(context.Background(), &cloudpb.ExchangeCredentialRequest{
		DeviceId: "101", Credential: "wrong",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestRecoveryResumesAfterGenerateFailureWithoutDeletingTwice(t *testing.T) {
	db := newDeviceAuthTestDB(t)
	token := seedDeviceAuthToken(t, db, true)
	fake := &fakeHilbertDeviceClient{apiKey: "rotated-key", getErr: errors.New("missing"), generateErr: errors.New("temporary")}
	identity := testDeviceIdentity(db, fake)
	service := &deviceInitService{identity: identity}
	req := &cloudpb.ReinitDeviceRequest{DeviceId: "101", DeviceAuthToken: token, Platform: "ios"}

	_, err := service.ReinitDevice(context.Background(), req)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("first recovery code = %v, want Unavailable", status.Code(err))
	}
	if fake.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", fake.deleteCalls)
	}
	fake.generateErr = nil
	fake.getErr = nil
	response, err := service.ReinitDevice(context.Background(), req)
	if err != nil {
		t.Fatalf("retry recovery error = %v", err)
	}
	if response.GetApiKey() != "rotated-key" || fake.deleteCalls != 1 {
		t.Fatalf("response=%#v delete_calls=%d", response, fake.deleteCalls)
	}
	var stage string
	if err := db.Get(&stage, `SELECT recovery_stage FROM ws_client_auth_tokens WHERE robot_id = 1 AND revoked_at IS NULL`); err != nil {
		t.Fatalf("query recovery stage: %v", err)
	}
	if stage != "completed" {
		t.Fatalf("stage = %q, want completed", stage)
	}
}

func testDeviceIdentity(db *sqlx.DB, hilbert hilbertDeviceClient) *deviceIdentityService {
	return &deviceIdentityService{
		db: db,
		cfg: Config{
			DeviceJWTSecret: "test-device-secret-at-least-32-bytes",
			DeviceJWTTTL:    15 * time.Minute,
			HilbertCode:     "svc",
			HilbertPassword: "secret",
		},
		hilbert: hilbert,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

func newDeviceAuthTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY, device_id TEXT NOT NULL, workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL, auth_epoch INTEGER NOT NULL, updated_at TIMESTAMP, deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE ws_client_auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT, robot_id INTEGER NOT NULL, token_hash TEXT NOT NULL,
			token_version TEXT NOT NULL, created_at TIMESTAMP, last_used_at TIMESTAMP NULL,
			sdk_initialized_at TIMESTAMP NULL, recovery_requested_at TIMESTAMP NULL,
			recovery_stage TEXT NOT NULL DEFAULT 'none', recovery_completed_at TIMESTAMP NULL,
			revoked_at TIMESTAMP NULL
		)`,
		`INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch) VALUES (1, '101', 10, 'active', 1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create fixture: %v\nquery=%s", err, statement)
		}
	}
	return db
}

func seedDeviceAuthToken(t *testing.T, db *sqlx.DB, recovery bool) string {
	t.Helper()
	token, err := services.GenerateDeviceAuthToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	requestedAt := any(nil)
	stage := "none"
	if recovery {
		requestedAt = time.Unix(1_800_000_000, 0).UTC()
		stage = "authorized"
	}
	if _, err := db.Exec(`
		INSERT INTO ws_client_auth_tokens (
			robot_id, token_hash, token_version, created_at, recovery_requested_at, recovery_stage
		) VALUES (1, ?, ?, ?, ?, ?)
	`, services.HashDeviceAuthToken(token), services.DeviceAuthTokenVersion, time.Now().UTC(), requestedAt, stage); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return token
}
