// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package orbit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClientSubmitsImmutableArgvJob(t *testing.T) {
	var received SubmitRequest
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/jobs/" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"job_id":"abs-job-derivative-1","submission_id":"derivative-1"}`)),
			Request:    r,
		}, nil
	})

	client, err := NewClient("http://orbit.example", time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.http.Transport = transport
	request := SubmitRequest{
		SubmissionID: "derivative-1",
		Image:        "ghcr.io/archebase/stereo-split@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Command:      []string{"python3", "/app/run_processing.py"},
		Args:         []string{"--kind", "stereo_split"},
		DataBindings: []DataBinding{{URI: "tos://bucket/raw/source.mcap", Path: "/bindings/input/source.mcap", Mode: "read"}},
	}
	response, err := client.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if response.JobID != "abs-job-derivative-1" || response.SubmissionID != request.SubmissionID {
		t.Fatalf("Submit() response = %+v", response)
	}
	if received.Entrypoint != "" || len(received.Command) != 2 || len(received.Args) != 2 {
		t.Fatalf("received request = %+v", received)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
