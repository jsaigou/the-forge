// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"encoding/json"

	"github.com/jsaigou/the-forge/internal/store"
)

// SettingToken is the settings-KV key holding the HF access token
// (docs/v5-store-schema.md's secret rule; store.go's Settings doc comment
// tracks this key's history — deleted as dead in migration 0029,
// reintroduced here with a real reader). Never logged, never placed in an
// error string, masked on every API read.
const SettingToken = "hf.token"

// TokenConfig is the operator-editable HF auth configuration.
type TokenConfig struct {
	Token string `json:"token"`
}

// LoadTokenConfig reads hf.token. A missing or malformed row is a valid
// "no token configured" state, not an error — gated repos simply fail
// closed with ErrGated until an operator sets one.
func LoadTokenConfig(ctx context.Context, s store.Settings) TokenConfig {
	if s == nil {
		return TokenConfig{}
	}
	raw, err := s.Get(ctx, SettingToken)
	if err != nil || len(raw) == 0 {
		return TokenConfig{}
	}
	var cfg TokenConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return TokenConfig{}
	}
	return cfg
}

// SaveTokenConfig writes hf.token as raw JSON.
func SaveTokenConfig(ctx context.Context, s store.Settings, cfg TokenConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.Set(ctx, SettingToken, raw)
}

// TokenFunc returns a live token-lookup closure suitable for Client.Token —
// each call re-reads settings, so a token change takes effect on the very
// next request without restarting anything holding the Client. Uses a
// fresh background context per call rather than closing over the caller's:
// Client is typically constructed once at daemon startup and this closure
// then outlives whatever request context existed at that moment.
func TokenFunc(s store.Settings) func() string {
	return func() string { return LoadTokenConfig(context.Background(), s).Token }
}
