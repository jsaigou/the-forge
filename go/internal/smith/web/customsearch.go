// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// customSearchAdapter — a generic search adapter (operator feedback
// 2026-08-14): GET {base_url}?q=<query> with Authorization: Bearer <key> if
// set. The operator supplies the full search endpoint; the response is
// parsed as a SearxNG-compatible {"results":[{title,url,content}]} JSON
// shape, so any SearxNG-compatible endpoint (a self-hosted instance, a
// wrapper, etc.) works without a per-service adapter. For search APIs with
// different response shapes, add a named adapter instead.
type customSearchAdapter struct {
	client    httpClient
	userAgent string
}

type customSearchResponse struct {
	Results []struct {
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Content string  `json:"content"`
		Score   float64 `json:"score"`
	} `json:"results"`
}

func (a *customSearchAdapter) search(ctx context.Context, cfg ProviderConfig, q string, limit int) ([]Result, Attempt) {
	start := time.Now()
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, Attempt{Provider: "customsearch", OK: false, Detail: "bad base_url: " + err.Error()}
	}
	qv := u.Query()
	qv.Set("q", q)
	u.RawQuery = qv.Encode()

	headers := map[string]string{}
	if cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.APIKey
	}
	res, err := doGet(ctx, a.client, a.userAgent, u.String(), headers, searchTimeout)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return nil, Attempt{Provider: "customsearch", OK: false, Detail: err.Error(), DurationMS: dur}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, Attempt{Provider: "customsearch", OK: false, Detail: fmt.Sprintf("status %d", res.StatusCode), Status: res.StatusCode, DurationMS: dur}
	}
	var parsed customSearchResponse
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, Attempt{Provider: "customsearch", OK: false, Detail: "malformed json: " + err.Error(), Status: res.StatusCode, DurationMS: dur}
	}
	if len(parsed.Results) == 0 {
		return nil, Attempt{Provider: "customsearch", OK: false, Detail: "no results", Status: res.StatusCode, DurationMS: dur}
	}

	n := min(limit, len(parsed.Results))
	out := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		r := parsed.Results[i]
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content, Engine: "customsearch", Score: r.Score})
	}
	return out, Attempt{Provider: "customsearch", OK: true, Status: res.StatusCode, DurationMS: dur}
}
