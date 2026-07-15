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
	hilbertRawDataRegisterPath             = "/v1/data-collection/raw-data/register"
	hilbertRawDataGetUploadCredentialsPath = "/v1/data-collection/raw-data/get-upload-credentials"
	hilbertRawDataFinishUploadPath         = "/v1/data-collection/raw-data/finish-upload"
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
}

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
		SessionToken    string    `json:"session_token"`
		ExpireTime      time.Time `json:"expire_time"`
	} `json:"credentials"`
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
