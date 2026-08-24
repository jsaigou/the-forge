// SPDX-License-Identifier: Apache-2.0

package comfyui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// clientTimeout bounds every ComfyUI API call. Loopback-only (this
// package's whole reason to exist separately from internal/smith/web — see
// the package doc), so 10s is generous, not a real-internet-fetch budget.
const clientTimeout = 10 * time.Second

// bodyCapBytes bounds how much of a response this package will read —
// /object_info on an installation with many custom nodes can be large, but
// unbounded is still wrong.
const bodyCapBytes = 4 << 20 // 4 MiB

// Client is the ComfyUI API surface BuildMap needs. An interface (not a
// concrete *HTTPClient) so map_test.go can inject fixtures without a real
// server — the same reason internal/smith/web keeps its adapters behind
// searcher/fetcher rather than a single concrete type.
type Client interface {
	Healthy(ctx context.Context) bool
	Queue(ctx context.Context) (QueueResponse, error)
	History(ctx context.Context) (map[string]HistoryEntry, error)
	ObjectInfo(ctx context.Context) (map[string]ObjectInfoEntry, error)
}

// HTTPClient is the production Client — a plain loopback HTTP client
// against ComfyUI's own API, own http.Client (never smith.Deps.HTTPClient
// or internal/smith/web's SSRF-hardened client — see the package doc).
type HTTPClient struct {
	BaseURL string
	http    *http.Client
}

// NewHTTPClient returns a Client against baseURL (e.g.
// "http://127.0.0.1:3001", smith.comfyui.url).
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{BaseURL: baseURL, http: &http.Client{Timeout: clientTimeout}}
}

func (c *HTTPClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("comfyui: build request %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("comfyui: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comfyui: GET %s: status %d", path, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyCapBytes))
	if err != nil {
		return fmt.Errorf("comfyui: read %s body: %w", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("comfyui: decode %s: %w", path, err)
	}
	return nil
}

// Healthy probes GET /queue (ComfyUI has no dedicated /health route; an
// empty-but-200 queue response is the standard liveness signal other
// ComfyUI tooling uses).
func (c *HTTPClient) Healthy(ctx context.Context) bool {
	var q QueueResponse
	return c.get(ctx, "/queue", &q) == nil
}

func (c *HTTPClient) Queue(ctx context.Context) (QueueResponse, error) {
	var q QueueResponse
	err := c.get(ctx, "/queue", &q)
	return q, err
}

func (c *HTTPClient) History(ctx context.Context) (map[string]HistoryEntry, error) {
	var h map[string]HistoryEntry
	err := c.get(ctx, "/history", &h)
	return h, err
}

func (c *HTTPClient) ObjectInfo(ctx context.Context) (map[string]ObjectInfoEntry, error) {
	var oi map[string]ObjectInfoEntry
	err := c.get(ctx, "/object_info", &oi)
	return oi, err
}
