// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"context"
	"fmt"
	"strings"
)

// staticResolver is a test helper: one fixed address per mode, so a test can point a backend at an httptest server. Production only has the daemon resolver.
type staticResolver struct {
	urls map[VoiceMode]string
}

// NewStaticResolver returns the resolver for fixed addresses (any of them may be empty).
func NewStaticResolver(custom, design, base, kokoro string) Resolver {
	t := func(s string) string { return strings.TrimRight(s, "/") }
	return &staticResolver{urls: map[VoiceMode]string{ModeCustomVoice: t(custom), ModeVoiceDesign: t(design), ModeBase: t(base), ModeKokoro: t(kokoro)}}
}

func (s *staticResolver) Resolve(_ context.Context, mode VoiceMode, _ string) (Resolved, error) {
	u, known := s.urls[mode]
	if !known {
		return Resolved{}, fmt.Errorf("unsupported mode %s", mode)
	}
	if u == "" {
		switch mode {
		case ModeBase:
			return Resolved{}, fmt.Errorf("no tts-server configured for base/clone")
		default:
			return Resolved{}, fmt.Errorf("no tts-server configured for %s", mode)
		}
	}
	return Resolved{URL: u, Release: func() {}}, nil
}

func (s *staticResolver) Status() map[string]any { return map[string]any{"resolver": "static"} }

// staticServerBackend and staticKokoroBackend build the production backends over fixed addresses, for tests that stand in for an engine with an httptest
// server (any address may be empty: that mode then answers a plain error).
func staticServerBackend(custom, design, base string) *serverBackend {
	return NewServerBackend(NewStaticResolver(custom, design, base, ""))
}

func staticKokoroBackend(url, token string) *kokoroBackend {
	return NewKokoroBackend(NewStaticResolver("", "", "", url), token)
}
