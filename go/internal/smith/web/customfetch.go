// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// customFetchAdapter — a generic fetch adapter (operator feedback 2026-08-14):
// GET {base_url}?url=<target> with Authorization: Bearer <key> if set. The
// operator supplies a fetch-proxy endpoint (a Jina-reader-style service, a
// self-hosted extractor, etc.); the response body is treated as extracted
// text (markdown/plain) and returned verbatim. For fetch services with
// structured JSON responses, add a named adapter instead.
type customFetchAdapter struct {
	client    httpClient
	userAgent string
}

func (a *customFetchAdapter) fetch(ctx context.Context, cfg ProviderConfig, targetURL string) (*Document, Attempt) {
	start := time.Now()
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, Attempt{Provider: "customfetch", OK: false, Detail: "bad base_url: " + err.Error()}
	}
	qv := u.Query()
	qv.Set("url", targetURL)
	u.RawQuery = qv.Encode()

	headers := map[string]string{}
	if cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.APIKey
	}
	res, err := doGet(ctx, a.client, a.userAgent, u.String(), headers, fetchTimeout)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return nil, Attempt{Provider: "customfetch", OK: false, Detail: err.Error(), DurationMS: dur}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, Attempt{Provider: "customfetch", OK: false, Detail: fmt.Sprintf("status %d", res.StatusCode), Status: res.StatusCode, DurationMS: dur}
	}
	text := string(res.Body)
	if strings.TrimSpace(text) == "" {
		return nil, Attempt{Provider: "customfetch", OK: false, Detail: "empty result", Status: res.StatusCode, DurationMS: dur}
	}
	ct := res.ContentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return &Document{
		URL:         targetURL,
		Title:       firstLine(text),
		Text:        text,
		ContentType: ct,
		StatusCode:  res.StatusCode,
		Provider:    "customfetch",
		Truncated:   res.Truncated,
	}, Attempt{Provider: "customfetch", OK: true, Status: res.StatusCode, DurationMS: dur}
}

// firstLine returns a trimmed single-line title heuristic from a markdown/text
// blob (a leading "# Heading" or the first non-empty line), so a custom fetch
// proxy that returns a markdown title first surfaces something useful.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		t = strings.TrimPrefix(t, "#")
		return strings.TrimSpace(t)
	}
	return ""
}
