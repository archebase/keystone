// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import "strings"

const callbackPathPrefix = "/api/v1/callbacks/"

type callbackURLs struct {
	baseURL string
}

func newCallbackURLs(baseURL string) callbackURLs {
	return callbackURLs{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
}

func (u callbackURLs) configured() bool {
	return u.baseURL != ""
}

func (u callbackURLs) startURL() string {
	return u.baseURL + callbackPathPrefix + "start"
}

func (u callbackURLs) finishURL() string {
	return u.baseURL + callbackPathPrefix + "finish"
}
