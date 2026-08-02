// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package orbit provides the narrow HTTP adapter Keystone uses to execute
// frozen derivative Jobs.
package orbit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxErrorBodyBytes = 16 * 1024

// ErrNotFound indicates that Orbit no longer has the requested Job.
var ErrNotFound = errors.New("orbit job not found")

// ErrConflict indicates that Orbit rejected a conflicting submission or state transition.
var ErrConflict = errors.New("orbit job conflict")

// DataBinding mounts one object-store URI at a path in an Orbit Job.
type DataBinding struct {
	URI  string `json:"uri"`
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
}

// Resources contains Kubernetes resource requests and limits for an Orbit Job.
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// SubmitRequest is the frozen Job specification sent to Orbit.
type SubmitRequest struct {
	SubmissionID         string            `json:"submission_id,omitempty"`
	Image                string            `json:"image"`
	Entrypoint           string            `json:"entrypoint,omitempty"`
	Command              []string          `json:"command,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	Env                  map[string]string `json:"env,omitempty"`
	WorkingDir           string            `json:"working_dir,omitempty"`
	DataBindings         []DataBinding     `json:"data_bindings,omitempty"`
	Resources            Resources         `json:"resources,omitempty"`
	TTLSecondsAfterDone  *int32            `json:"ttl_seconds_after_done,omitempty"`
	BackoffLimit         *int32            `json:"backoff_limit,omitempty"`
	ActiveDeadlineSecond *int64            `json:"active_deadline_seconds,omitempty"`
}

// SubmitResponse identifies the accepted Orbit Job and idempotent submission.
type SubmitResponse struct {
	JobID        string `json:"job_id"`
	SubmissionID string `json:"submission_id"`
}

// Job is the Orbit controller's current view of one submitted Job.
type Job struct {
	JobID        string        `json:"job_id"`
	SubmissionID string        `json:"submission_id"`
	Status       string        `json:"status"`
	Message      string        `json:"message,omitempty"`
	Image        string        `json:"image,omitempty"`
	Entrypoint   string        `json:"entrypoint,omitempty"`
	DataBindings []DataBinding `json:"data_bindings,omitempty"`
	CreatedAt    *time.Time    `json:"created_at,omitempty"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
}

// HTTPError reports a non-success response returned by Orbit.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("orbit returned HTTP %d: %s", e.StatusCode, e.Message)
}

// Client calls one configured Orbit controller.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient validates an Orbit controller URL and constructs its HTTP adapter.
func NewClient(rawBaseURL string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil {
		return nil, fmt.Errorf("parse Orbit base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid Orbit base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("orbit base URL must use http or https")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(parsed.String(), "/"),
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Submit creates or recovers one idempotently identified Orbit Job.
func (c *Client) Submit(ctx context.Context, request SubmitRequest) (SubmitResponse, error) {
	var response SubmitResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/jobs/", request, &response, http.StatusAccepted); err != nil {
		return SubmitResponse{}, err
	}
	if strings.TrimSpace(response.JobID) == "" || strings.TrimSpace(response.SubmissionID) == "" {
		return SubmitResponse{}, fmt.Errorf("orbit submit response is missing job identity")
	}
	return response, nil
}

// Get returns the current state of an Orbit Job.
func (c *Client) Get(ctx context.Context, id string) (Job, error) {
	var response Job
	if err := c.doJSON(ctx, http.MethodGet, jobPath(id), nil, &response, http.StatusOK); err != nil {
		return Job{}, err
	}
	return response, nil
}

// Logs returns the controller's log text for an Orbit Job.
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	var response struct {
		Logs string `json:"logs"`
	}
	if err := c.doJSON(ctx, http.MethodGet, jobPath(id)+"/logs", nil, &response, http.StatusOK); err != nil {
		return "", err
	}
	return response.Logs, nil
}

// Stop requests cancellation of an Orbit Job.
func (c *Client) Stop(ctx context.Context, id string) (Job, error) {
	var response Job
	if err := c.doJSON(ctx, http.MethodPost, jobPath(id)+"/stop", struct{}{}, &response, http.StatusOK); err != nil {
		return Job{}, err
	}
	return response, nil
}

// Delete asks Orbit to remove the Job and its data-binding resources.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, jobPath(id), nil, nil, http.StatusNoContent)
}

func jobPath(id string) string {
	return "/api/jobs/" + url.PathEscape(strings.TrimSpace(id))
}

func (c *Client) doJSON(ctx context.Context, method, requestPath string, body any, response any, expectedStatus int) error {
	if c == nil || c.http == nil || c.baseURL == "" {
		return fmt.Errorf("orbit client is not configured")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Orbit request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, reader)
	if err != nil {
		return fmt.Errorf("create Orbit request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call Orbit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != expectedStatus {
		message := orbitErrorMessage(resp.Body)
		httpErr := &HTTPError{StatusCode: resp.StatusCode, Message: message}
		switch resp.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrNotFound, httpErr)
		case http.StatusConflict:
			return fmt.Errorf("%w: %w", ErrConflict, httpErr)
		default:
			return httpErr
		}
	}
	if response == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024))
	if err := decoder.Decode(response); err != nil {
		return fmt.Errorf("decode Orbit response: %w", err)
	}
	return nil
}

func orbitErrorMessage(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error)
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		return http.StatusText(http.StatusInternalServerError)
	}
	return message
}
