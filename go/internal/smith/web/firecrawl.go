// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// firecrawlAdapter — POST {base}/v1/scrape {"url":...,"formats":["markdown"]}.
// Live-verified against ForgeHost's self-hosted firecrawl, 2026-08-11: no API
// key required, returns clean markdown — no HTML parsing needed on this
// path (that's what makes firecrawl worth trying before `direct`).
type firecrawlAdapter struct {
	client    httpClient
	userAgent string
}

type firecrawlRequest struct {
	URL     string   `json:"url"`
	Formats []string `json:"formats"`
}

type firecrawlResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Markdown string `json:"markdown"`
		Metadata struct {
			Title       string `json:"title"`
			SourceURL   string `json:"sourceURL"`
			StatusCode  int    `json:"statusCode"`
			ContentType string `json:"contentType"`
		} `json:"metadata"`
	} `json:"data"`
	Error string `json:"error"`
}

func (a *firecrawlAdapter) fetch(ctx context.Context, cfg ProviderConfig, targetURL string) (*Document, Attempt) {
	start := time.Now()
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/scrape"
	headers := map[string]string{}
	if cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.APIKey
	}
	res, err := doPostJSON(ctx, a.client, a.userAgent, endpoint,
		firecrawlRequest{URL: targetURL, Formats: []string{"markdown"}}, headers, fetchTimeout)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return nil, Attempt{Provider: "firecrawl", OK: false, Detail: err.Error(), DurationMS: dur}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, Attempt{Provider: "firecrawl", OK: false, Detail: fmt.Sprintf("status %d", res.StatusCode), Status: res.StatusCode, DurationMS: dur}
	}
	var parsed firecrawlResponse
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, Attempt{Provider: "firecrawl", OK: false, Detail: "malformed json: " + err.Error(), Status: res.StatusCode, DurationMS: dur}
	}
	if !parsed.Success || strings.TrimSpace(parsed.Data.Markdown) == "" {
		detail := parsed.Error
		if detail == "" {
			detail = "empty result"
		}
		return nil, Attempt{Provider: "firecrawl", OK: false, Detail: detail, Status: res.StatusCode, DurationMS: dur}
	}
	doc := &Document{
		URL:         targetURL,
		Title:       parsed.Data.Metadata.Title,
		Text:        parsed.Data.Markdown,
		ContentType: parsed.Data.Metadata.ContentType,
		StatusCode:  parsed.Data.Metadata.StatusCode,
		Provider:    "firecrawl",
		Truncated:   res.Truncated,
	}
	return doc, Attempt{Provider: "firecrawl", OK: true, Status: res.StatusCode, DurationMS: dur}
}
