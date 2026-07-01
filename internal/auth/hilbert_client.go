// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/config"
)

const (
	hilbertNoncePath = "/v1/console/nonce/generate"
	hilbertLoginPath = "/v1/console/account/login"

	hilbertNonceKeyLengthBytes = 32
	hilbertNonceIVLengthBytes  = 12
	hilbertNonceLengthBytes    = hilbertNonceKeyLengthBytes + hilbertNonceIVLengthBytes
)

var (
	// ErrHilbertInvalidCredentials indicates Hilbert rejected the account credentials or account policy.
	ErrHilbertInvalidCredentials = errors.New("hilbert invalid credentials")
	// ErrHilbertUnavailable indicates Hilbert could not complete authentication because the service failed.
	ErrHilbertUnavailable = errors.New("hilbert unavailable")
)

// HilbertAccount stores the sanitized account fields Keystone needs after Hilbert login.
type HilbertAccount struct {
	// ID is the Hilbert account primary key.
	ID int64 `json:"id"`

	// Code is the Hilbert account login identifier and maps to Keystone operator_id.
	Code string `json:"code"`

	// DisplayName is the human-readable Hilbert account name copied into Keystone display fields.
	DisplayName string `json:"displayName"`

	// Role is the Hilbert role code, such as external_user.
	Role string `json:"role"`

	// ExternalUserType is the Hilbert external subtype, such as data_supplier.
	ExternalUserType string `json:"externalUserType"`

	// Status is the Hilbert account status, such as enabled.
	Status string `json:"status"`
}

// HilbertLoginResult is the successful authentication result returned to Keystone handlers.
type HilbertLoginResult struct {
	// Account stores the Hilbert account that authenticated successfully.
	Account HilbertAccount
}

// HilbertClient authenticates collector credentials against the Hilbert backend.
type HilbertClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewHilbertClient creates a Hilbert authentication client from Keystone auth configuration.
func NewHilbertClient(cfg *config.AuthConfig) *HilbertClient {
	if cfg == nil {
		return &HilbertClient{httpClient: &http.Client{Timeout: 5 * time.Second}}
	}
	timeoutSeconds := cfg.HilbertTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	return &HilbertClient{
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.HilbertBaseURL), "/"),
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

// Configured reports whether the client has enough endpoint configuration to call Hilbert.
func (c *HilbertClient) Configured() bool {
	return c != nil && strings.TrimSpace(c.baseURL) != ""
}

// Login authenticates one Hilbert account code and plaintext password.
func (c *HilbertClient) Login(ctx context.Context, code string, password string) (*HilbertLoginResult, error) {
	if !c.Configured() {
		return nil, ErrHilbertUnavailable
	}

	nonceRecord, err := c.generateNonce(ctx)
	if err != nil {
		return nil, err
	}

	cipherDigest, err := encryptHilbertPasswordDigest(password, nonceRecord.RandomKey)
	if err != nil {
		return nil, fmt.Errorf("%w: encrypt password digest", ErrHilbertUnavailable)
	}

	return c.loginWithCipherDigest(ctx, code, nonceRecord.ID, cipherDigest)
}

type hilbertCommonResponse[T any] struct {
	Code int `json:"code"`
	Data T   `json:"data"`
}

type hilbertNonceData struct {
	ID        int64  `json:"id"`
	RandomKey string `json:"randomKey"`
}

type hilbertLoginData struct {
	Account HilbertAccount `json:"account"`
	//nolint:gosec // Hilbert's response contract names this field sessionKey; Keystone only verifies it is present and never stores it.
	SessionKey string `json:"sessionKey"`
}

func (c *HilbertClient) generateNonce(ctx context.Context) (*hilbertNonceData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertNoncePath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create nonce request", ErrHilbertUnavailable)
	}

	var resp hilbertCommonResponse[hilbertNonceData]
	if err := c.doJSON(req, &resp); err != nil {
		if errors.Is(err, ErrHilbertInvalidCredentials) {
			return nil, fmt.Errorf("%w: nonce request rejected", ErrHilbertUnavailable)
		}
		return nil, err
	}
	if resp.Code != 0 || resp.Data.ID == 0 || strings.TrimSpace(resp.Data.RandomKey) == "" {
		return nil, fmt.Errorf("%w: nonce response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return &resp.Data, nil
}

func (c *HilbertClient) loginWithCipherDigest(ctx context.Context, code string, nonceID int64, cipherDigest string) (*HilbertLoginResult, error) {
	body, err := json.Marshal(map[string]any{
		"code":         code,
		"nonceId":      nonceID,
		"cipherDigest": cipherDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal login request", ErrHilbertUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+hilbertLoginPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create login request", ErrHilbertUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")

	var resp hilbertCommonResponse[hilbertLoginData]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: login response code %d", ErrHilbertInvalidCredentials, resp.Code)
	}
	if strings.TrimSpace(resp.Data.SessionKey) == "" {
		return nil, fmt.Errorf("%w: missing session key", ErrHilbertUnavailable)
	}
	return &HilbertLoginResult{Account: resp.Data.Account}, nil
}

func (c *HilbertClient) doJSON(req *http.Request, out any) (err error) {
	resp, err := c.httpClient.Do(req) //nolint:gosec // Hilbert base URL is operator-configured backend infrastructure, not user-controlled request input.
	if err != nil {
		return fmt.Errorf("%w: request failed: %v", ErrHilbertUnavailable, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("%w: close response body: %v", ErrHilbertUnavailable, closeErr)
		}
	}()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: status %d", ErrHilbertInvalidCredentials, resp.StatusCode)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: status %d", ErrHilbertUnavailable, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrHilbertUnavailable, err)
	}
	return nil
}

func encryptHilbertPasswordDigest(password string, encodedMaterial string) (string, error) {
	digest := sha256.Sum256([]byte(password))
	plainDigest := hex.EncodeToString(digest[:])

	material, err := base64.StdEncoding.DecodeString(encodedMaterial)
	if err != nil {
		return "", fmt.Errorf("decode nonce material: %w", err)
	}
	if len(material) != hilbertNonceLengthBytes {
		return "", fmt.Errorf("nonce material length must be %d bytes", hilbertNonceLengthBytes)
	}

	block, err := aes.NewCipher(material[:hilbertNonceKeyLengthBytes])
	if err != nil {
		return "", fmt.Errorf("create aes cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create aes-gcm cipher: %w", err)
	}

	cipherText := aesGCM.Seal(nil, material[hilbertNonceKeyLengthBytes:], []byte(plainDigest), nil)
	return base64.StdEncoding.EncodeToString(cipherText), nil
}
