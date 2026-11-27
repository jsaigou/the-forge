package duplexremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

// A Locator finds the engine for one session, loading it on demand, and keeps it from being evicted while the session lives (ADR-0029). The fixed
// Config.BaseURL is the development mode; a Locator is the production one: Forge's pool owns where the engine runs and when it is stopped.

// Lease is an engine address together with the lease that protects it.
type Lease struct {
	BaseURL string
	ID      string
	// Expires is when the lease lapses unless it is renewed.
	Expires time.Time
}

// ErrLeaseGone means the lease no longer exists (released, expired, or its engine was stopped and started again): the engine under the session is no
// longer protected, so the session must end.
var ErrLeaseGone = errors.New("duplexremote: the engine lease is gone")

// Locator resolves and leases an engine. Locate may take as long as an engine start (about a minute for Haru): it returns when the engine is up.
type Locator interface {
	// Locate returns a lease on the pool engine named engine, loading it if needed. The returned error is a *duplex.Error when the platform should
	// show it to the client as is.
	Locate(ctx context.Context, engine string) (Lease, error)
	// Renew extends the lease; ErrLeaseGone means it is gone for good, any other error may be transient.
	Renew(ctx context.Context, id string) error
	// Release gives the lease back. Best effort and idempotent: the lease also expires by itself.
	Release(ctx context.Context, id string)
}

// DaemonLocator is the Locator backed by the Forge daemon's duplex endpoints (POST /api/v1/duplex/resolve, renew, release).
type DaemonLocator struct {
	// BaseURL is the daemon's API base, e.g. http://127.0.0.1:5000.
	BaseURL string
	// Key is a duplex-scoped Forge API key. It is sent only as a header and never logged or put in an error message.
	Key string
	// Client is used for every call. It must not have a short overall timeout: a resolve waits for an engine start. nil means a plain client.
	Client *http.Client
}

func (d *DaemonLocator) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{}
}

func (d *DaemonLocator) post(ctx context.Context, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.BaseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.Key != "" {
		req.Header.Set("Authorization", "Bearer "+d.Key)
	}
	return d.client().Do(req)
}

type daemonError struct {
	Error  string         `json:"error"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params"`
}

// Locate asks the daemon to resolve and lease the engine, which loads it if it is not running.
func (d *DaemonLocator) Locate(ctx context.Context, engine string) (Lease, error) {
	resp, err := d.post(ctx, "/api/v1/duplex/resolve", map[string]string{"engine": engine})
	if err != nil {
		if ctx.Err() != nil {
			return Lease{}, &duplex.Error{Code: duplex.ErrCancelled, Message: "start cancelled while the engine was loading", Fatal: true}
		}
		return Lease{}, unavailable("the Forge daemon is unreachable")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxControlSize))
	if resp.StatusCode != http.StatusOK {
		return Lease{}, locateError(resp.StatusCode, data)
	}
	var out struct {
		Engine    string `json:"engine"`
		BaseURL   string `json:"base_url"`
		LeaseID   string `json:"lease_id"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.BaseURL == "" || out.LeaseID == "" {
		return Lease{}, unavailable("the Forge daemon answered resolve with an unusable body")
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		// Without an expiry there is no renewal schedule. Do not guess: give the lease back and fail.
		d.Release(context.WithoutCancel(ctx), out.LeaseID)
		return Lease{}, unavailable("the Forge daemon answered resolve without a usable lease expiry")
	}
	return Lease{BaseURL: out.BaseURL, ID: out.LeaseID, Expires: exp}, nil
}

// locateError maps the daemon's stable codes onto platform errors. Transient or not is the daemon's call and is in the message the client reads.
func locateError(status int, data []byte) error {
	var de daemonError
	_ = json.Unmarshal(data, &de)
	switch de.Code {
	case "duplex_no_engine":
		return &duplex.Error{Code: duplex.ErrTenantUnavailable, Fatal: true, Message: "no speech-to-speech engine is registered under that name"}
	case "duplex_engine_off":
		return &duplex.Error{Code: duplex.ErrTenantUnavailable, Fatal: true, Message: "that engine is turned off; turn it on in the Forge Engines tab"}
	case "specialist_pool_full", "duplex_lease_limit":
		return &duplex.Error{Code: duplex.ErrSessionLimit, Fatal: true, Message: "no room to start the engine right now; try again in a moment"}
	case "specialist_unavailable":
		return &duplex.Error{Code: duplex.ErrModelNotReady, Fatal: true, Message: "the engine could not be started in time; try again"}
	case "auth", "forbidden", "unauthorized":
		return &duplex.Error{Code: duplex.ErrAuth, Fatal: true, Message: "the Forge daemon refused this service's key"}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &duplex.Error{Code: duplex.ErrAuth, Fatal: true, Message: "the Forge daemon refused this service's key"}
	}
	return unavailable("the Forge daemon could not provide an engine (HTTP %d)", status)
}

// Renew extends the lease. 410 is ErrLeaseGone; anything else that fails is reported as is and may be retried.
func (d *DaemonLocator) Renew(ctx context.Context, id string) error {
	resp, err := d.post(ctx, "/api/v1/duplex/renew", map[string]string{"lease_id": id})
	if err != nil {
		return fmt.Errorf("renew: daemon unreachable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxControlSize))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusGone:
		return ErrLeaseGone
	default:
		return fmt.Errorf("renew: daemon answered HTTP %d", resp.StatusCode)
	}
}

// Release gives the lease back, best effort.
func (d *DaemonLocator) Release(ctx context.Context, id string) {
	resp, err := d.post(ctx, "/api/v1/duplex/release", map[string]string{"lease_id": id})
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxControlSize))
}
