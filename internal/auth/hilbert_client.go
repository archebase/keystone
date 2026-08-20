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
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/config"
)

const (
	hilbertNoncePath              = "/v1/console/nonce/generate"
	hilbertLoginPath              = "/v1/console/account/login"
	hilbertAccountQueryPath       = "/v1/console/account/query"
	hilbertAccountGetCurPath      = "/v1/console/account/get-cur"
	hilbertWorkspaceAvailablePath = "/v1/console/workspace/list-available"
	hilbertDCPlanQueryPath        = "/v1/data-collection/dc-plan/query"
	hilbertDCPlanPatchDevicePath  = "/v1/data-collection/dc-plan/patch-dc-device-id"
	hilbertDCDeviceQueryPath      = "/v1/data-collection/dc-device/query"
	hilbertDCDeviceGetKeyPath     = "/v1/data-collection/dc-device/get-api-key"
	hilbertDCDeviceGeneratePath   = "/v1/data-collection/dc-device/generate-api-key"
	hilbertDCDeviceDeletePath     = "/v1/data-collection/dc-device/delete-api-key"
	hilbertDCDeviceValidatePath   = "/v1/data-collection/dc-device/validate"
	hilbertDCDeviceTypeQueryPath  = "/v1/data-collection/dc-device-type/query"
	hilbertNonceConsumePath       = "/v1/console/nonce/consume"

	hilbertNonceKeyLengthBytes = 32
	hilbertNonceIVLengthBytes  = 12
	hilbertNonceLengthBytes    = hilbertNonceKeyLengthBytes + hilbertNonceIVLengthBytes

	hilbertDCDeviceQueryPageSize int64 = 200
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

	sessionKey string
}

// NewHilbertLoginResult creates a Hilbert login result without exporting the bearer token field.
func NewHilbertLoginResult(account HilbertAccount, sessionKey string) *HilbertLoginResult {
	return &HilbertLoginResult{Account: account, sessionKey: sessionKey}
}

// SessionKey returns the bearer token Hilbert returned for subsequent API calls.
func (r *HilbertLoginResult) SessionKey() string {
	if r == nil {
		return ""
	}
	return r.sessionKey
}

// HilbertWorkspace stores the Hilbert workspace projection Keystone caches locally.
type HilbertWorkspace struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	Admins      []string   `json:"admins"`
	Members     []string   `json:"members"`
	CreatedTime time.Time  `json:"createdTime"`
	UpdatedTime *time.Time `json:"updatedTime"`
}

// HilbertDCPlan stores one Hilbert data collection plan projection.
type HilbertDCPlan struct {
	ID                   int64             `json:"id"`
	WorkspaceID          int64             `json:"workspaceId"`
	Name                 string            `json:"name"`
	Description          *string           `json:"description"`
	DCFactoryID          int64             `json:"dcFactoryId"`
	DCServiceProviderID  int64             `json:"dcServiceProviderId"`
	Operator             string            `json:"operator"`
	OperatorDisplayName  string            `json:"operatorDisplayName,omitempty"`
	DCProjectID          int64             `json:"dcProjectId"`
	DCProjectName        string            `json:"dcProjectName,omitempty"`
	DCProjectDescription string            `json:"dcProjectDescription,omitempty"`
	DCProject            *HilbertDCPlanRef `json:"dcProject,omitempty"`
	DCTaskID             int64             `json:"dcTaskId"`
	DCTaskName           string            `json:"dcTaskName,omitempty"`
	DCTaskDescription    string            `json:"dcTaskDescription,omitempty"`
	DCTask               *HilbertDCPlanRef `json:"dcTask,omitempty"`
	DCDeviceID           *int64            `json:"dcDeviceId"`
	DCDeviceName         string            `json:"dcDeviceName,omitempty"`
	DCDevice             *HilbertDCPlanRef `json:"dcDevice,omitempty"`
	DCType               string            `json:"dcType"`
	DCDate               string            `json:"dcDate"`
	TargetCount          int64             `json:"targetCount"`
	CurCount             int64             `json:"curCount"`
	TargetDuration       int64             `json:"targetDuration"`
	CurDuration          int64             `json:"curDuration"`
	CreatedBy            string            `json:"createdBy"`
	CreatedTime          time.Time         `json:"createdTime"`
	UpdatedBy            *string           `json:"updatedBy"`
	UpdatedTime          *time.Time        `json:"updatedTime"`
}

// HilbertDCPlanRef stores the association shape embedded in Hilbert dc_plan responses.
type HilbertDCPlanRef struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

// UnmarshalJSON accepts Hilbert's nested dcProject/dcTask association objects
// and the older flat name aliases used by some deployments.
func (p *HilbertDCPlan) UnmarshalJSON(data []byte) error {
	type alias HilbertDCPlan
	aux := struct {
		*alias
		ProjectName string `json:"projectName"`
		ProjectDesc string `json:"projectDescription"`
		TaskName    string `json:"taskName"`
		TaskDesc    string `json:"taskDescription"`
		DeviceName  string `json:"deviceName"`
	}{
		alias: (*alias)(p),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if p.DCProjectName == "" {
		p.DCProjectName = aux.ProjectName
	}
	if p.DCProjectName == "" && p.DCProject != nil {
		p.DCProjectName = p.DCProject.Name
	}
	if p.DCProjectDescription == "" {
		p.DCProjectDescription = aux.ProjectDesc
	}
	if p.DCProjectDescription == "" && p.DCProject != nil && p.DCProject.Description != nil {
		p.DCProjectDescription = *p.DCProject.Description
	}
	if p.DCTaskName == "" {
		p.DCTaskName = aux.TaskName
	}
	if p.DCTaskName == "" && p.DCTask != nil {
		p.DCTaskName = p.DCTask.Name
	}
	if p.DCTaskDescription == "" {
		p.DCTaskDescription = aux.TaskDesc
	}
	if p.DCTaskDescription == "" && p.DCTask != nil && p.DCTask.Description != nil {
		p.DCTaskDescription = *p.DCTask.Description
	}
	if p.DCDeviceName == "" {
		p.DCDeviceName = aux.DeviceName
	}
	if p.DCDeviceName == "" && p.DCDevice != nil {
		p.DCDeviceName = p.DCDevice.Name
	}
	return nil
}

// HilbertDCPlanPage stores Hilbert's page wrapper for dc-plan query.
type HilbertDCPlanPage struct {
	Records  []HilbertDCPlan `json:"records"`
	Total    int64           `json:"total"`
	PageNum  int64           `json:"pageNum"`
	PageSize int64           `json:"pageSize"`
}

// HilbertAccountPage stores Hilbert's page wrapper for account query.
type HilbertAccountPage struct {
	Records  []HilbertAccount `json:"records"`
	Total    int64            `json:"total"`
	PageNum  int64            `json:"pageNum"`
	PageSize int64            `json:"pageSize"`
}

// HilbertDCDevice stores one Hilbert data collection device projection.
type HilbertDCDevice struct {
	ID             int64      `json:"id"`
	WorkspaceID    int64      `json:"workspaceId"`
	Name           string     `json:"name"`
	Description    *string    `json:"description"`
	SN             string     `json:"sn"`
	DCDeviceTypeID int64      `json:"dcDeviceTypeId"`
	CreatedBy      string     `json:"createdBy"`
	CreatedTime    time.Time  `json:"createdTime"`
	UpdatedBy      *string    `json:"updatedBy"`
	UpdatedTime    *time.Time `json:"updatedTime"`
}

// HilbertDCDevicePage stores Hilbert's page wrapper for dc-device query.
type HilbertDCDevicePage struct {
	Records  []HilbertDCDevice `json:"records"`
	Total    int64             `json:"total"`
	PageNum  int64             `json:"pageNum"`
	PageSize int64             `json:"pageSize"`
}

// HilbertDCDeviceType stores one Hilbert data collection device type projection.
type HilbertDCDeviceType struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	CreatedBy   string     `json:"createdBy"`
	CreatedTime time.Time  `json:"createdTime"`
	UpdatedBy   *string    `json:"updatedBy"`
	UpdatedTime *time.Time `json:"updatedTime"`
}

// HilbertDCDeviceTypePage stores Hilbert's page wrapper for dc-device-type query.
type HilbertDCDeviceTypePage struct {
	Records  []HilbertDCDeviceType `json:"records"`
	Total    int64                 `json:"total"`
	PageNum  int64                 `json:"pageNum"`
	PageSize int64                 `json:"pageSize"`
}

type hilbertDCDeviceAPIKey struct {
	NonceID      int64  `json:"nonceId"`
	CipherAPIKey string `json:"cipherApiKey"`
}

// HilbertClient authenticates collector credentials against the Hilbert backend.
type HilbertClient struct {
	baseURL    string
	accessKey  string
	secretKey  string
	httpClient *http.Client
	now        func() time.Time
}

// NewHilbertClient creates a Hilbert API client from Keystone Hilbert configuration.
func NewHilbertClient(cfg *config.HilbertConfig) *HilbertClient {
	if cfg == nil {
		return &HilbertClient{httpClient: &http.Client{Timeout: 5 * time.Second}, now: time.Now}
	}
	timeoutSeconds := cfg.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	return &HilbertClient{
		baseURL:   strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		accessKey: strings.TrimSpace(cfg.AccessKey),
		secretKey: strings.TrimSpace(cfg.SecretKey),
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
		now: time.Now,
	}
}

// Configured reports whether the client has enough endpoint configuration to call Hilbert.
func (c *HilbertClient) Configured() bool {
	return c != nil && strings.TrimSpace(c.baseURL) != ""
}

// ServiceAuthConfigured reports whether the client has endpoint and Digest AK/SK credentials for service calls.
func (c *HilbertClient) ServiceAuthConfigured() bool {
	return c.Configured() && strings.TrimSpace(c.accessKey) != "" && strings.TrimSpace(c.secretKey) != ""
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

// ListAvailableWorkspaces fetches workspaces available to the authenticated Hilbert session.
func (c *HilbertClient) ListAvailableWorkspaces(ctx context.Context) ([]HilbertWorkspace, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertWorkspaceAvailablePath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create workspace list request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[[]HilbertWorkspace]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: workspace list response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return resp.Data, nil
}

// GetCurrentAccount fetches the Hilbert account authenticated by the configured service AK/SK.
func (c *HilbertClient) GetCurrentAccount(ctx context.Context) (*HilbertAccount, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertAccountGetCurPath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create current account request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[HilbertAccount]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: current account response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return &resp.Data, nil
}

// QueryDCPlans fetches one page of Hilbert data collection plans for one workspace.
func (c *HilbertClient) QueryDCPlans(ctx context.Context, workspaceID int64, pageNum int64, pageSize int64) (*HilbertDCPlanPage, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	if workspaceID <= 0 || pageNum <= 0 || pageSize <= 0 {
		return nil, fmt.Errorf("%w: invalid dc plan query parameters", ErrHilbertUnavailable)
	}

	query := url.Values{}
	query.Set("workspaceId", strconv.FormatInt(workspaceID, 10))
	query.Set("pageNum", strconv.FormatInt(pageNum, 10))
	query.Set("pageSize", strconv.FormatInt(pageSize, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertDCPlanQueryPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create dc plan query request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[HilbertDCPlanPage]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: dc plan query response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return &resp.Data, nil
}

// PatchDCPlanDCDeviceID binds a Hilbert data collection plan to a device once.
func (c *HilbertClient) PatchDCPlanDCDeviceID(ctx context.Context, workspaceID, planID, deviceID int64) (bool, error) {
	if workspaceID <= 0 || planID <= 0 || deviceID <= 0 {
		return false, fmt.Errorf("%w: invalid dc plan device binding parameters", ErrHilbertUnavailable)
	}
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertDCPlanPatchDevicePath, map[string]int64{
		"workspaceId": workspaceID,
		"id":          planID,
		"dcDeviceId":  deviceID,
	})
	if err != nil {
		return false, err
	}
	var resp hilbertCommonResponse[bool]
	if err := c.doJSON(req, &resp); err != nil {
		return false, err
	}
	if resp.Code != 0 {
		message := resp.errorMessage()
		if message == "" {
			message = fmt.Sprintf("response code %d", resp.Code)
		}
		return false, fmt.Errorf("%w: patch dc plan device: %s", ErrHilbertUnavailable, message)
	}
	return resp.Data, nil
}

// QueryAccountByCode fetches one Hilbert account by exact account code.
func (c *HilbertClient) QueryAccountByCode(ctx context.Context, code string) (*HilbertAccount, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("%w: missing account code", ErrHilbertUnavailable)
	}

	query := url.Values{}
	query.Set("code", code)
	query.Set("pageNum", "1")
	query.Set("pageSize", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertAccountQueryPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create account query request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[HilbertAccountPage]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: account query response code %d", ErrHilbertUnavailable, resp.Code)
	}
	if len(resp.Data.Records) == 0 {
		return nil, nil
	}
	return &resp.Data.Records[0], nil
}

// QueryDCDevices fetches every Hilbert data collection device visible in one workspace.
func (c *HilbertClient) QueryDCDevices(ctx context.Context, workspaceID int64) (*HilbertDCDevicePage, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	if workspaceID <= 0 {
		return nil, fmt.Errorf("%w: invalid dc device workspace id", ErrHilbertUnavailable)
	}

	devices := make([]HilbertDCDevice, 0)
	var total int64
	for pageNum := int64(1); ; pageNum++ {
		page, err := c.queryDCDevicesPage(ctx, workspaceID, pageNum, hilbertDCDeviceQueryPageSize)
		if err != nil {
			return nil, err
		}
		devices = append(devices, page.Records...)
		if page.Total > total {
			total = page.Total
		}
		if int64(len(devices)) > total {
			total = int64(len(devices))
		}
		if len(page.Records) == 0 || int64(len(devices)) >= total {
			break
		}
	}

	return &HilbertDCDevicePage{
		Records:  devices,
		Total:    total,
		PageNum:  1,
		PageSize: hilbertDCDeviceQueryPageSize,
	}, nil
}

func (c *HilbertClient) queryDCDevicesPage(ctx context.Context, workspaceID int64, pageNum int64, pageSize int64) (*HilbertDCDevicePage, error) {
	query := url.Values{}
	query.Set("workspaceId", strconv.FormatInt(workspaceID, 10))
	query.Set("pageNum", strconv.FormatInt(pageNum, 10))
	query.Set("pageSize", strconv.FormatInt(pageSize, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertDCDeviceQueryPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create dc device query request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[HilbertDCDevicePage]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: dc device query response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return &resp.Data, nil
}

// QueryDCDeviceTypeByID fetches one Hilbert data collection device type by primary key.
func (c *HilbertClient) QueryDCDeviceTypeByID(ctx context.Context, id int64) (*HilbertDCDeviceType, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	if id <= 0 {
		return nil, fmt.Errorf("%w: invalid dc device type id", ErrHilbertUnavailable)
	}

	query := url.Values{}
	query.Set("id", strconv.FormatInt(id, 10))
	query.Set("pageNum", "1")
	query.Set("pageSize", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertDCDeviceTypeQueryPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create dc device type query request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[HilbertDCDeviceTypePage]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: dc device type query response code %d", ErrHilbertUnavailable, resp.Code)
	}
	if len(resp.Data.Records) == 0 {
		return nil, nil
	}
	return &resp.Data.Records[0], nil
}

// GetDCDeviceAPIKey fetches and decrypts the existing Hilbert device API key.
func (c *HilbertClient) GetDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64) (string, error) {
	query := url.Values{}
	query.Set("workspaceId", strconv.FormatInt(workspaceID, 10))
	query.Set("id", strconv.FormatInt(deviceID, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertDCDeviceGetKeyPath+"?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: create device API key request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return "", err
	}
	return c.readDeviceAPIKeyResponse(ctx, req)
}

// GenerateDCDeviceAPIKey creates and decrypts a Hilbert device API key.
func (c *HilbertClient) GenerateDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64) (string, error) {
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertDCDeviceGeneratePath, map[string]int64{
		"workspaceId": workspaceID,
		"id":          deviceID,
	})
	if err != nil {
		return "", err
	}
	return c.readDeviceAPIKeyResponse(ctx, req)
}

// DeleteDCDeviceAPIKey removes the Hilbert device API key.
func (c *HilbertClient) DeleteDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64) error {
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertDCDeviceDeletePath, map[string]int64{
		"workspaceId": workspaceID,
		"id":          deviceID,
	})
	if err != nil {
		return err
	}
	var resp hilbertCommonResponse[bool]
	if err := c.doJSON(req, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("%w: delete device API key response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return nil
}

// ValidateDCDeviceAPIKey validates a plaintext key through Hilbert's nonce transport.
func (c *HilbertClient) ValidateDCDeviceAPIKey(ctx context.Context, workspaceID, deviceID int64, apiKey string) (bool, error) {
	nonceRecord, err := c.generateNonce(ctx)
	if err != nil {
		return false, err
	}
	cipherAPIKey, err := EncryptHilbertNonceValue(apiKey, nonceRecord.RandomKey)
	if err != nil {
		return false, fmt.Errorf("%w: encrypt device API key", ErrHilbertUnavailable)
	}
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertDCDeviceValidatePath, map[string]any{
		"workspaceId":  workspaceID,
		"id":           deviceID,
		"nonceId":      nonceRecord.ID,
		"cipherApiKey": cipherAPIKey,
	})
	if err != nil {
		return false, err
	}
	var resp hilbertCommonResponse[bool]
	if err := c.doJSON(req, &resp); err != nil {
		return false, err
	}
	if resp.Code != 0 {
		return false, fmt.Errorf("%w: validate device API key response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return resp.Data, nil
}

func (c *HilbertClient) readDeviceAPIKeyResponse(ctx context.Context, req *http.Request) (string, error) {
	var resp hilbertCommonResponse[hilbertDCDeviceAPIKey]
	if err := c.doJSON(req, &resp); err != nil {
		return "", err
	}
	if resp.Code != 0 || resp.Data.NonceID <= 0 || strings.TrimSpace(resp.Data.CipherAPIKey) == "" {
		return "", fmt.Errorf("%w: device API key response code %d", ErrHilbertUnavailable, resp.Code)
	}
	nonceRecord, err := c.consumeNonce(ctx, resp.Data.NonceID)
	if err != nil {
		return "", err
	}
	return DecryptHilbertNonceValue(resp.Data.CipherAPIKey, nonceRecord.RandomKey)
}

func (c *HilbertClient) consumeNonce(ctx context.Context, nonceID int64) (*hilbertNonceData, error) {
	query := url.Values{}
	query.Set("id", strconv.FormatInt(nonceID, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertNonceConsumePath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create nonce consume request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}
	var resp hilbertCommonResponse[*hilbertNonceData]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 || resp.Data == nil {
		return nil, fmt.Errorf("%w: nonce consume response code %d", ErrHilbertUnavailable, resp.Code)
	}
	return resp.Data, nil
}

func (c *HilbertClient) hilbertServiceJSONRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Hilbert request", ErrHilbertUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: create Hilbert request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (c *HilbertClient) authorizeServiceRequest(req *http.Request) error {
	now := time.Now
	if c != nil && c.now != nil {
		now = c.now
	}
	header, err := c.serviceAuthorizationHeader(now())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", header)
	return nil
}

func (c *HilbertClient) serviceAuthorizationHeader(now time.Time) (string, error) {
	ak := strings.TrimSpace(c.accessKey)
	sk := strings.TrimSpace(c.secretKey)
	if ak == "" || sk == "" {
		return "", fmt.Errorf("%w: missing Hilbert AK/SK", ErrHilbertInvalidCredentials)
	}
	millis := strconv.FormatInt(now.UnixMilli(), 10)
	digest := sha256.Sum256([]byte(sk + "," + millis))
	return "Digest " + ak + ";" + millis + ";" + hex.EncodeToString(digest[:]), nil
}

type hilbertCommonResponse[T any] struct {
	Code    int             `json:"code"`
	Message json.RawMessage `json:"message"`
	Msg     json.RawMessage `json:"msg"`
	Data    T               `json:"data"`
}

func (r hilbertCommonResponse[T]) errorMessage() string {
	if msg := hilbertMessageText(r.Message); msg != "" {
		return msg
	}
	return hilbertMessageText(r.Msg)
}

func hilbertMessageText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var localized map[string]string
	if err := json.Unmarshal(raw, &localized); err == nil {
		if strings.TrimSpace(localized["zh_CN"]) != "" {
			return strings.TrimSpace(localized["zh_CN"])
		}
		if strings.TrimSpace(localized["en_US"]) != "" {
			return strings.TrimSpace(localized["en_US"])
		}
		for _, value := range localized {
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return strings.TrimSpace(string(raw))
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
	return NewHilbertLoginResult(resp.Data.Account, resp.Data.SessionKey), nil
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
		return fmt.Errorf("%w: status %d body %q", ErrHilbertInvalidCredentials, resp.StatusCode, limitedResponseBody(resp))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: status %d body %q", ErrHilbertUnavailable, resp.StatusCode, limitedResponseBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrHilbertUnavailable, err)
	}
	return nil
}

func limitedResponseBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.TrimSpace(string(data))
}

func encryptHilbertPasswordDigest(password string, encodedMaterial string) (string, error) {
	digest := sha256.Sum256([]byte(password))
	plainDigest := hex.EncodeToString(digest[:])
	return EncryptHilbertNonceValue(plainDigest, encodedMaterial)
}

// EncryptHilbertNonceValue encrypts one plaintext value with Hilbert nonce material.
func EncryptHilbertNonceValue(plainText string, encodedMaterial string) (string, error) {
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

	cipherText := aesGCM.Seal(nil, material[hilbertNonceKeyLengthBytes:], []byte(plainText), nil)
	return base64.StdEncoding.EncodeToString(cipherText), nil
}

// DecryptHilbertNonceValue decrypts one Hilbert nonce-encrypted value.
func DecryptHilbertNonceValue(encodedCipherText string, encodedMaterial string) (string, error) {
	material, err := base64.StdEncoding.DecodeString(encodedMaterial)
	if err != nil {
		return "", fmt.Errorf("decode nonce material: %w", err)
	}
	if len(material) != hilbertNonceLengthBytes {
		return "", fmt.Errorf("nonce material length must be %d bytes", hilbertNonceLengthBytes)
	}
	cipherText, err := base64.StdEncoding.DecodeString(encodedCipherText)
	if err != nil {
		return "", fmt.Errorf("decode cipher text: %w", err)
	}
	block, err := aes.NewCipher(material[:hilbertNonceKeyLengthBytes])
	if err != nil {
		return "", fmt.Errorf("create aes cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create aes-gcm cipher: %w", err)
	}
	plainText, err := aesGCM.Open(nil, material[hilbertNonceKeyLengthBytes:], cipherText, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt cipher text: %w", err)
	}
	return string(plainText), nil
}
