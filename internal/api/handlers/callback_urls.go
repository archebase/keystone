// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/url"
	"strings"
)

const callbackPathPrefix = "/api/v1/callbacks/"

// CallbackAllowlist describes the callback URL scope Axon is allowed to call.
type CallbackAllowlist struct {
	AllowedHost       string `json:"allowed_host"`
	AllowedPathPrefix string `json:"allowed_path_prefix"`
}

type callbackURLs struct {
	baseURL string
}

func newCallbackURLs(baseURL string) callbackURLs {
	return callbackURLs{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
}

func (u callbackURLs) configured() bool {
	return u.baseURL != ""
}

func (u callbackURLs) allowlist() CallbackAllowlist {
	parsed, err := url.Parse(u.baseURL)
	if err != nil {
		return CallbackAllowlist{AllowedPathPrefix: callbackPathPrefix}
	}
	return CallbackAllowlist{
		AllowedHost:       parsed.Host,
		AllowedPathPrefix: callbackPathPrefix,
	}
}

func (u callbackURLs) startURL() string {
	return u.baseURL + callbackPathPrefix + "start"
}

func (u callbackURLs) finishURL() string {
	return u.baseURL + callbackPathPrefix + "finish"
}
