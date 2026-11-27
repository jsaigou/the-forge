// SPDX-License-Identifier: Apache-2.0

package router

// Upstream failure classification (ADR-0020 F5, docs/api-contract-implementation-plan.md phase 3).
//
// a0 used to treat every upstream status >= 500 as "this backend is broken": fail over, count a slot error, and
// finally answer 502. llama-server answers a request it can never serve (an image sent to a text-only model) with
// a 500 or 501 and a structured body. That is a verdict on the
// request, not on the backend: retrying elsewhere cannot help, counting it as a slot error pollutes health, and a
// 502 hides the real message. Only a structured, deterministic request error is passed through (as 400, with the
// upstream body untouched); a 500 with no such body, a reset connection, and 502/503/504 still fail over.
//
// What the real server sends (measured on plain upstream build 19, image to a text-only model): HTTP 500,
// `{"error":{"code":500,"message":"image input is not supported - hint: ...","type":"server_error"}}`. The type is
// the generic server_error, so the message is the only signal; other builds use type not_supported_error. Both are
// recognised.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
)

// unsupportedInputMessage matches llama-server's refusal of an input kind the loaded model cannot take.
var unsupportedInputMessage = regexp.MustCompile(`(?i)\b(image|audio|video) input is not supported\b`)

// headerUpstreamStatus carries the status the upstream really sent when a0 re-maps it.
const headerUpstreamStatus = "X-Forge-Upstream-Status"

// upstreamErrorPeekLimit bounds how much of an error body is read to classify it.
const upstreamErrorPeekLimit = 64 << 10

// deterministicErrorTypes are upstream error types that describe the request, not the backend.
var deterministicErrorTypes = map[string]bool{
	"not_supported_error":   true,
	"invalid_request_error": true,
}

// deterministicUpstreamError reports whether resp is a 500/501 whose JSON body is a deterministic request error,
// and returns its type. It reads at most upstreamErrorPeekLimit bytes and always leaves resp.Body readable with
// the full original content.
func deterministicUpstreamError(resp *http.Response) (string, bool) {
	if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusNotImplemented {
		return "", false
	}
	if resp.Body == nil {
		return "", false
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorPeekLimit))
	resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), closer: resp.Body}
	var doc struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(head, &doc) != nil || len(doc.Error) == 0 || doc.Error[0] != '{' {
		return "", false
	}
	var e struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(doc.Error, &e) != nil {
		return "", false
	}
	if deterministicErrorTypes[e.Type] {
		return e.Type, true
	}
	if unsupportedInputMessage.MatchString(e.Message) {
		return "unsupported_input", true
	}
	return "", false
}

type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b *replayBody) Close() error { return b.closer.Close() }
