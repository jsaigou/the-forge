// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// directAdapter is the always-present fetch terminus: a guarded GET plus a
// stdlib-tokenizer HTML→text extraction. Used whenever firecrawl is down,
// disabled, or not configured. Its client (constructed in New via
// newGuardedHTTPClient) rejects loopback/private/link-local/CGNAT
// destinations — direct fetches arbitrary URLs a search result chose, so it
// cannot reach tailnet/LAN hosts (accepted consequence, docs/v5-smith.md
// §4.8 plan R4).
type directAdapter struct {
	client    httpClient
	userAgent string
}

func (a *directAdapter) fetch(ctx context.Context, _ ProviderConfig, targetURL string) (*Document, Attempt) {
	start := time.Now()
	res, err := doGet(ctx, a.client, a.userAgent, targetURL, nil, fetchTimeout)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return nil, Attempt{Provider: "direct", OK: false, Detail: err.Error(), DurationMS: dur}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, Attempt{Provider: "direct", OK: false, Detail: fmt.Sprintf("status %d", res.StatusCode), Status: res.StatusCode, DurationMS: dur}
	}
	ct := res.ContentType
	isHTML := strings.Contains(ct, "text/html") || ct == ""
	isText := strings.Contains(ct, "text/plain")
	isJSON := strings.Contains(ct, "application/json")
	if !isHTML && !isText && !isJSON {
		return nil, Attempt{Provider: "direct", OK: false, Detail: "unsupported content-type " + ct, Status: res.StatusCode, DurationMS: dur}
	}

	var title, text string
	if isHTML {
		title, text = extractHTMLText(res.Body)
	} else {
		text = string(res.Body)
	}
	if strings.TrimSpace(text) == "" {
		return nil, Attempt{Provider: "direct", OK: false, Detail: "empty extracted text", Status: res.StatusCode, DurationMS: dur}
	}
	doc := &Document{
		URL:         targetURL,
		Title:       title,
		Text:        text,
		ContentType: ct,
		StatusCode:  res.StatusCode,
		Provider:    "direct",
		Truncated:   res.Truncated,
	}
	return doc, Attempt{Provider: "direct", OK: true, Status: res.StatusCode, DurationMS: dur}
}

// extractHTMLText walks the parsed DOM, skipping non-content elements, and
// returns (title, body text). html.Parse is a lenient, spec-compliant
// tokenizer that never errors on real-world malformed markup — there is no
// error path to handle here.
func extractHTMLText(raw []byte) (title, text string) {
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return "", ""
	}
	skip := map[string]bool{"script": true, "style": true, "noscript": true, "nav": true, "footer": true}
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "title" && n.FirstChild != nil && title == "" {
				title = strings.TrimSpace(n.FirstChild.Data)
			}
			if skip[n.Data] {
				return
			}
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				sb.WriteString(t)
				sb.WriteString(" ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, collapseWhitespace(sb.String())
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
