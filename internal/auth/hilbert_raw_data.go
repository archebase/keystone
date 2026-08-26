// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	hilbertRawDataRegisterPath                     = "/v1/data-collection/raw-data/register"
	hilbertRawDataQueryPath                        = "/v1/data-collection/raw-data/query"
	hilbertRawDataGetUploadCredentialsPath         = "/v1/data-collection/raw-data/get-upload-credentials" // #nosec G101 -- API path, not a credential.
	hilbertRawDataFinishUploadPath                 = "/v1/data-collection/raw-data/finish-upload"
	hilbertParamFileRegisterPath                   = "/v1/data-collection/raw-data/register-param-file"
	hilbertParamFileGetUploadCredentialsPath       = "/v1/data-collection/raw-data/get-param-file-upload-credentials" // #nosec G101 -- API path, not a credential.
	hilbertParamFileFinishUploadPath               = "/v1/data-collection/raw-data/finish-param-file-upload"
	hilbertRawDataQueryPageSize              int64 = 200
)

// HilbertRawDataRegisterRequest contains the fields Hilbert requires before it
// issues object-storage upload credentials for one raw MCAP file.
type HilbertRawDataRegisterRequest struct {
	WorkspaceID  int64     `json:"workspaceId"`
	DCPlanID     int64     `json:"dcPlanId"`
	BagName      string    `json:"bagName"`
	BagStartTime time.Time `json:"bagStartTime"`
	BagEndTime   time.Time `json:"bagEndTime"`
	BagSize      int64     `json:"bagSize"`
	BagDigest    string    `json:"bagDigest"`
	// ParamFileMotionStoreID optionally binds a completed Hilbert CalibrationSnapshot.
	ParamFileMotionStoreID *string `json:"paramFileMotionStoreId,omitempty"`
}

// HilbertParamFileRegisterRequest contains the metadata Hilbert requires before
// issuing credentials for a calibration.json CalibrationSnapshot.
type HilbertParamFileRegisterRequest struct {
	WorkspaceID   int64  `json:"workspaceId"`
	ContentSHA256 string `json:"contentSha256"`
	SizeBytes     int64  `json:"sizeBytes"`
}

// HilbertRawData contains the immutable registration fields Keystone verifies
// before adopting a row after an ambiguous register response.
type HilbertRawData struct {
	ID           int64     `json:"id"`
	WorkspaceID  int64     `json:"workspaceId"`
	DCPlanID     int64     `json:"dcPlanId"`
	BagName      string    `json:"bagName"`
	BagStartTime time.Time `json:"bagStartTime"`
	BagEndTime   time.Time `json:"bagEndTime"`
	BagSize      int64     `json:"bagSize"`
	BagDigest    string    `json:"bagDigest"`
}

type hilbertRawDataPage struct {
	Records  []HilbertRawData `json:"records"`
	Total    int64            `json:"total"`
	PageNum  int64            `json:"pageNum"`
	PageSize int64            `json:"pageSize"`
}

// HilbertParamFileUploadCredentials uses the same object-storage credential
// contract as raw-data uploads.
type HilbertParamFileUploadCredentials = HilbertRawDataUploadCredentials

// HilbertRawDataUploadCredentials is the object-storage target and temporary
// credential set returned by Hilbert.
type HilbertRawDataUploadCredentials struct {
	Provider    string `json:"provider"`
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Credentials struct {
		AccessKeyID     string    `json:"access_key_id"`
		SecretAccessKey string    `json:"secret_access_key"`
		TemporaryToken  string    `json:"session_token"` // #nosec G117 -- Hilbert API field name is session_token for temporary TOS auth.
		ExpireTime      time.Time `json:"expire_time"`
	} `json:"credentials"`
}

// HilbertParamFileRegistration describes a CalibrationSnapshot registration returned by Hilbert.
type HilbertParamFileRegistration struct {
	ParamFileMotionStoreID string `json:"paramFileMotionStoreId"`
	State                  string `json:"state"`
}

const (
	// CalibrationSnapshotStateUploading indicates that the calibration object still needs to be uploaded.
	CalibrationSnapshotStateUploading = "UPLOADING"
	// CalibrationSnapshotStateReady indicates that the calibration object is complete and bindable.
	CalibrationSnapshotStateReady = "READY"
)

// RegisterParamFile creates or resolves a Hilbert CalibrationSnapshot resource and returns its state.
func (c *HilbertClient) RegisterParamFile(ctx context.Context, request HilbertParamFileRegisterRequest) (*HilbertParamFileRegistration, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	if request.WorkspaceID <= 0 || request.SizeBytes <= 0 || len(strings.TrimSpace(request.ContentSHA256)) != 64 {
		return nil, fmt.Errorf("%w: invalid param-file register request", ErrHilbertUnavailable)
	}
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertParamFileRegisterPath, request)
	if err != nil {
		return nil, err
	}
	var resp hilbertCommonResponse[HilbertParamFileRegistration]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 || strings.TrimSpace(resp.Data.ParamFileMotionStoreID) == "" || strings.TrimSpace(resp.Data.State) == "" {
		return nil, fmt.Errorf("%w: param-file register response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
	}
	return &resp.Data, nil
}

// GetParamFileUploadCredentials fetches temporary storage credentials for a registered CalibrationSnapshot.
func (c *HilbertClient) GetParamFileUploadCredentials(ctx context.Context, workspaceID int64, paramFileID string) (*HilbertParamFileUploadCredentials, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	query := url.Values{}
	query.Set("workspaceId", strconv.FormatInt(workspaceID, 10))
	query.Set("paramFileMotionStoreId", strings.TrimSpace(paramFileID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertParamFileGetUploadCredentialsPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create param-file credentials request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}
	var resp hilbertCommonResponse[HilbertParamFileUploadCredentials]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: param-file credentials response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
	}
	if strings.TrimSpace(resp.Data.Bucket) == "" || strings.TrimSpace(resp.Data.Key) == "" {
		return nil, fmt.Errorf("%w: param-file credentials response missing storage fields", ErrHilbertUnavailable)
	}
	return &resp.Data, nil
}

// FinishParamFileUpload marks a CalibrationSnapshot object as completely uploaded.
func (c *HilbertClient) FinishParamFileUpload(ctx context.Context, workspaceID int64, paramFileID string) error {
	if !c.ServiceAuthConfigured() {
		return ErrHilbertUnavailable
	}
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertParamFileFinishUploadPath, map[string]interface{}{"workspaceId": workspaceID, "paramFileMotionStoreId": paramFileID})
	if err != nil {
		return err
	}
	var resp hilbertCommonResponse[bool]
	if err := c.doJSON(req, &resp); err != nil {
		return err
	}
	if resp.Code != 0 || !resp.Data {
		return fmt.Errorf("%w: param-file finish response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
	}
	return nil
}

// RegisterRawData creates a Hilbert raw-data row and returns its rawDataId.
func (c *HilbertClient) RegisterRawData(ctx context.Context, request HilbertRawDataRegisterRequest) (int64, error) {
	if !c.ServiceAuthConfigured() {
		return 0, ErrHilbertUnavailable
	}
	if request.WorkspaceID <= 0 || request.DCPlanID <= 0 || strings.TrimSpace(request.BagName) == "" || request.BagSize <= 0 || strings.TrimSpace(request.BagDigest) == "" {
		return 0, fmt.Errorf("%w: invalid raw-data register request", ErrHilbertUnavailable)
	}
	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertRawDataRegisterPath, request)
	if err != nil {
		return 0, err
	}

	var resp hilbertCommonResponse[int64]
	if err := c.doJSON(req, &resp); err != nil {
		return 0, err
	}
	if resp.Code != 0 || resp.Data <= 0 {
		return 0, fmt.Errorf("%w: raw-data register response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
	}
	return resp.Data, nil
}

// FindRawDataByBagName scans Hilbert's existing fuzzy query endpoint and
// returns only the exact globally unique bag name in the requested workspace.
func (c *HilbertClient) FindRawDataByBagName(ctx context.Context, workspaceID int64, bagName string) (*HilbertRawData, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	bagName = strings.TrimSpace(bagName)
	if workspaceID <= 0 || bagName == "" {
		return nil, fmt.Errorf("%w: invalid raw-data lookup request", ErrHilbertUnavailable)
	}

	for pageNum := int64(1); ; pageNum++ {
		query := url.Values{}
		query.Set("workspaceId", strconv.FormatInt(workspaceID, 10))
		query.Set("bagName", bagName)
		query.Set("pageNum", strconv.FormatInt(pageNum, 10))
		query.Set("pageSize", strconv.FormatInt(hilbertRawDataQueryPageSize, 10))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertRawDataQueryPath+"?"+query.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("%w: create raw-data lookup request", ErrHilbertUnavailable)
		}
		if err := c.authorizeServiceRequest(req); err != nil {
			return nil, err
		}

		var resp hilbertCommonResponse[hilbertRawDataPage]
		if err := c.doJSON(req, &resp); err != nil {
			return nil, err
		}
		if resp.Code != 0 {
			return nil, fmt.Errorf("%w: raw-data lookup response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
		}
		for index := range resp.Data.Records {
			if resp.Data.Records[index].BagName == bagName {
				record := resp.Data.Records[index]
				return &record, nil
			}
		}
		if pageNum*hilbertRawDataQueryPageSize >= resp.Data.Total {
			return nil, nil
		}
		if len(resp.Data.Records) == 0 {
			return nil, fmt.Errorf("%w: raw-data lookup returned an empty page before total was exhausted", ErrHilbertUnavailable)
		}
	}
}

// GetRawDataUploadCredentials fetches temporary object-storage credentials for
// an already registered raw-data row.
func (c *HilbertClient) GetRawDataUploadCredentials(ctx context.Context, workspaceID, rawDataID int64) (*HilbertRawDataUploadCredentials, error) {
	if !c.ServiceAuthConfigured() {
		return nil, ErrHilbertUnavailable
	}
	if workspaceID <= 0 || rawDataID <= 0 {
		return nil, fmt.Errorf("%w: invalid raw-data credentials request", ErrHilbertUnavailable)
	}

	query := url.Values{}
	query.Set("workspaceId", strconv.FormatInt(workspaceID, 10))
	query.Set("id", strconv.FormatInt(rawDataID, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+hilbertRawDataGetUploadCredentialsPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create raw-data credentials request", ErrHilbertUnavailable)
	}
	if err := c.authorizeServiceRequest(req); err != nil {
		return nil, err
	}

	var resp hilbertCommonResponse[HilbertRawDataUploadCredentials]
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: raw-data credentials response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
	}
	if strings.TrimSpace(resp.Data.Bucket) == "" || strings.TrimSpace(resp.Data.Key) == "" || strings.TrimSpace(resp.Data.Credentials.AccessKeyID) == "" || strings.TrimSpace(resp.Data.Credentials.SecretAccessKey) == "" {
		return nil, fmt.Errorf("%w: raw-data credentials response missing storage fields", ErrHilbertUnavailable)
	}
	return &resp.Data, nil
}

// FinishRawDataUpload tells Hilbert that Keystone has uploaded the object to the
// temporary object-storage destination.
func (c *HilbertClient) FinishRawDataUpload(ctx context.Context, workspaceID, rawDataID int64) error {
	if !c.ServiceAuthConfigured() {
		return ErrHilbertUnavailable
	}
	if workspaceID <= 0 || rawDataID <= 0 {
		return fmt.Errorf("%w: invalid raw-data finish request", ErrHilbertUnavailable)
	}

	req, err := c.hilbertServiceJSONRequest(ctx, http.MethodPost, hilbertRawDataFinishUploadPath, map[string]int64{
		"workspaceId": workspaceID,
		"rawDataId":   rawDataID,
	})
	if err != nil {
		return err
	}
	var resp hilbertCommonResponse[bool]
	if err := c.doJSON(req, &resp); err != nil {
		return err
	}
	if resp.Code != 0 || !resp.Data {
		return fmt.Errorf("%w: raw-data finish response code %d message %q", ErrHilbertUnavailable, resp.Code, resp.errorMessage())
	}
	return nil
}
