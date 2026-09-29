// Package cli — client for The Forge dashboard API, used by the `forge`
// CLI verbs and TUI. Talks HTTP to the local daemon (:5000 by default)
// with a bearer sk-forge key.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/i18n"
)

// Client is a minimal typed client for the dashboard API.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

// DefaultBaseURL is the local daemon's dashboard listener.
const DefaultBaseURL = "http://127.0.0.1:5000"

// KeyPath is where a minted CLI key lives (forge keys export writes it).
func KeyPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "forge", "cli.key")
	}
	return ""
}

// New resolves base URL + key from env / keyfile.
//
//	FORGE_API_URL  (default http://127.0.0.1:5000)
//	FORGE_API_KEY  (else ~/.config/forge/cli.key)
func New() (*Client, error) {
	base := os.Getenv("FORGE_API_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("FORGE_API_URL must be an http(s) URL with a host, got %q", base)
	}

	key := os.Getenv("FORGE_API_KEY")
	if key == "" {
		if p := KeyPath(); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				key = strings.TrimSpace(string(b))
			}
		}
	}
	if key == "" {
		return nil, fmt.Errorf("no API key: set FORGE_API_KEY or run `forge keys export` (writes %s)", KeyPath())
	}
	return &Client{
		BaseURL: base,
		Key:     key,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *Client) do(method, path string, body any) ([]byte, error) {
	// Paths are API routes composed onto BaseURL; reject anything that is
	// not a clean absolute path so a caller bug can never redirect the
	// request to another host or inject header CRLFs.
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00") {
		return nil, fmt.Errorf("cli: invalid API path %q", path)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return data, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, apiErrorMessage(data))
	}
	return data, nil
}

// apiErrorMessage renders an API error response body for a human reading
// the CLI/TUI (Phase 4, multilanguage plan). Prefers a translated
// "errors.<code>" lookup when the body carries a `code` (writeErrorCode's
// JSON shape, go/internal/httpapi/httpapi.go) AND the Go i18n catalog has
// that code (Exists — only a subset of the backend's codes is mirrored
// here, same posture as web/src/lib/api.ts's apiErrorMessage), falling back
// to the body's own (always-English) `error` field, and finally to the raw
// truncated body when it isn't valid JSON at all (a proxy error page, a
// panic dump, etc.) — never showing an empty message.
func apiErrorMessage(data []byte) string {
	var body struct {
		Error  string         `json:"error"`
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &body); err == nil {
		if body.Code != "" {
			if key := "errors." + body.Code; i18n.Exists(key) {
				return i18n.TParams(key, body.Params)
			}
		}
		if body.Error != "" {
			return body.Error
		}
	}
	msg := strings.TrimSpace(string(data))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// GetJSON fetches and decodes into v.
func (c *Client) GetJSON(path string, v any) error {
	data, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// PostJSON posts and decodes the response into v (may be nil).
func (c *Client) PostJSON(path string, body, v any) error {
	data, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if v != nil && len(data) > 0 {
		return json.Unmarshal(data, v)
	}
	return nil
}

func (c *Client) DeleteJSON(path string, v any) error {
	data, err := c.do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if v != nil && len(data) > 0 {
		return json.Unmarshal(data, v)
	}
	return nil
}
