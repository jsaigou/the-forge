package cli

import (
	"fmt"
)

// Status returns the combined status payload.
func (c *Client) Status() (*Status, error) {
	var s Status
	if err := c.GetJSON("/api/v1/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Metrics returns live host/GPU metrics.
func (c *Client) Metrics() (*Metrics, error) {
	var m Metrics
	if err := c.GetJSON("/api/v1/metrics", &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SchedulerStatus returns the fleet/queue/memory snapshot.
func (c *Client) SchedulerStatus() (*SchedulerStatus, error) {
	var s SchedulerStatus
	if err := c.GetJSON("/api/v1/scheduler/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ConfigCards lists catalog configs (loadable models).
func (c *Client) ConfigCards() ([]ConfigCard, error) {
	var v struct {
		Cards []ConfigCard `json:"cards"`
	}
	if err := c.GetJSON("/api/v1/configs/cards", &v); err != nil {
		return nil, err
	}
	return v.Cards, nil
}

// Load loads a mode/config onto a slot.
func (c *Client) Load(mode, slot string) (*LifecycleResult, error) {
	var r LifecycleResult
	err := c.PostJSON("/api/v1/load", LoadRequest{Mode: mode, Slot: slot}, &r)
	return &r, err
}

// Unload empties a slot.
func (c *Client) Unload(slot string) (*LifecycleResult, error) {
	var r LifecycleResult
	err := c.PostJSON("/api/v1/unload", UnloadRequest{Slot: slot}, &r)
	return &r, err
}

// Switch switches the primary slot to a named mode.
func (c *Client) Switch(mode string) error {
	return c.PostJSON(fmt.Sprintf("/api/v1/switch/%s", mode), map[string]any{}, nil)
}

// InfraServices lists always-on + service-mode services.
func (c *Client) InfraServices() (*InfraServices, error) {
	var s InfraServices
	if err := c.GetJSON("/api/v1/infra-services", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ServiceStart starts a service-mode unit.
func (c *Client) ServiceStart(name string) error {
	return c.PostJSON(fmt.Sprintf("/api/v1/service-mode/%s/start", name), map[string]any{}, nil)
}

// ServiceStop stops a service-mode unit.
func (c *Client) ServiceStop(name string) error {
	return c.PostJSON(fmt.Sprintf("/api/v1/service-mode/%s/stop", name), map[string]any{}, nil)
}

// Notifications lists active alerts/notices.
func (c *Client) Notifications() (*NotificationsResponse, error) {
	var n NotificationsResponse
	if err := c.GetJSON("/api/v1/notifications", &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// CompressorSummary returns compressor savings stats for a window.
func (c *Client) CompressorSummary(window string) (*CompressorSummary, error) {
	var s CompressorSummary
	w := window
	if w == "" {
		w = "24h"
	}
	if err := c.GetJSON("/api/v1/compressor/summary?window="+w, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Providers lists remote provider credentials.
func (c *Client) Providers() (*ProvidersResponse, error) {
	var p ProvidersResponse
	if err := c.GetJSON("/api/v1/providers", &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Keys lists minted dashboard API keys.
func (c *Client) Keys() (*KeysResponse, error) {
	var k KeysResponse
	if err := c.GetJSON("/api/v1/keys", &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// KeyCreate mints a new key; the token is returned once. bindToRequester
// binds the key to this request's own resolved client IP (#34); ttlSeconds
// expires it that many seconds from now (0 = never, #36).
func (c *Client) KeyCreate(kind, name, role string, bindToRequester bool, ttlSeconds int64) (*KeyCreateResponse, error) {
	var r KeyCreateResponse
	req := KeyCreateRequest{Kind: kind, Name: name, BindToRequester: bindToRequester, TTLSeconds: ttlSeconds}
	if role != "" {
		req.Role = role
	}
	if err := c.PostJSON("/api/v1/keys", req, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// KeyRevoke revokes a key by id.
func (c *Client) KeyRevoke(keyID string) error {
	return c.DeleteJSON("/api/v1/keys/"+keyID, nil)
}

// SmithConversation fetches a conversation's messages.
func (c *Client) SmithConversation(id int64) (*SmithConversationDetail, error) {
	var d SmithConversationDetail
	err := c.GetJSON(fmt.Sprintf("/api/v1/smith/conversations/%d", id), &d)
	return &d, err
}

// SmithChat posts one chat turn; answer arrives via SSE smith:token deltas.
func (c *Client) SmithChat(conversationID int64, text string, escalate, web bool) (*SmithChatReply, error) {
	var r SmithChatReply
	body := SmithChatRequest{ConversationID: conversationID, Text: text, Escalate: escalate, Web: web}
	if conversationID == 0 {
		body.ConversationID = 0 // server creates a fresh conversation
	}
	err := c.PostJSON("/api/v1/smith/chat", body, &r)
	return &r, err
}
