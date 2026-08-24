// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// searxngAdapter — GET {base}/search?q=<q>&format=json. Live-verified
// against ForgeHost's self-hosted searxng, 2026-08-11: no API key required,
// clean JSON response.
type searxngAdapter struct {
	client    httpClient
	userAgent string
}

type searxngResponse struct {
	Results []struct {
		Title         string   `json:"title"`
		URL           string   `json:"url"`
		Content       string   `json:"content"`
		Engine        string   `json:"engine"`
		Engines       []string `json:"engines"`
		Score         float64  `json:"score"`
		Category      string   `json:"category"`
		PublishedDate *string  `json:"publishedDate"`
	} `json:"results"`
}

func (a *searxngAdapter) search(ctx context.Context, cfg ProviderConfig, q string, limit int) ([]Result, Attempt) {
	start := time.Now()
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/") + "/search")
	if err != nil {
		return nil, Attempt{Provider: "searxng", OK: false, Detail: "bad base_url: " + err.Error()}
	}
	qv := u.Query()
	qv.Set("q", q)
	qv.Set("format", "json")
	u.RawQuery = qv.Encode()

	headers := map[string]string{}
	if cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.APIKey
	}
	res, err := doGet(ctx, a.client, a.userAgent, u.String(), headers, searchTimeout)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return nil, Attempt{Provider: "searxng", OK: false, Detail: err.Error(), DurationMS: dur}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, Attempt{Provider: "searxng", OK: false, Detail: fmt.Sprintf("status %d", res.StatusCode), Status: res.StatusCode, DurationMS: dur}
	}
	var parsed searxngResponse
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, Attempt{Provider: "searxng", OK: false, Detail: "malformed json: " + err.Error(), Status: res.StatusCode, DurationMS: dur}
	}
	if len(parsed.Results) == 0 {
		return nil, Attempt{Provider: "searxng", OK: false, Detail: "no results", Status: res.StatusCode, DurationMS: dur}
	}

	n := min(limit, len(parsed.Results))
	out := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		r := parsed.Results[i]
		engine := r.Engine
		if engine == "" && len(r.Engines) > 0 {
			engine = r.Engines[0]
		}
		var pub *time.Time
		if r.PublishedDate != nil && *r.PublishedDate != "" {
			if t, err := time.Parse(time.RFC3339, *r.PublishedDate); err == nil {
				pub = &t
			}
		}
		out = append(out, Result{
			Title: r.Title, URL: r.URL, Snippet: r.Content, Engine: engine,
			Score: r.Score, PublishedAt: pub,
		})
	}
	return out, Attempt{Provider: "searxng", OK: true, Status: res.StatusCode, DurationMS: dur}
}
