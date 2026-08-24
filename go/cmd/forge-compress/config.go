// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/jsaigou/the-forge/internal/compress"
)

// config holds everything read from the environment at startup. Field names
// mirror the env var names this binary reads, most of which are the exact
// names internal/compressorctl.Provisioner already writes into a template
// instance's env file (OPENAI_TARGET_API_URL, COMPRESS_ONNX_INTRA_THREADS,
// COMPRESS_PROXY_TOKEN, COMPRESS_PORT) — kept unchanged so Provisioner needs
// no per-field rework to drive this binary, only the TemplatePrefix
// generalization the plan's "Store / provisioning / routing changes"
// section already calls for. The remaining vars (model/tokenizer/runtime
// paths) are new — this binary's own artifacts, constant across every
// instance on one host, so they're not written per-row by Provisioner.
type config struct {
	Port int
	// TargetAPIURL is the fallback upstream (bare origin + /v1, e.g.
	// "https://api.deepseek.com/v1") used when a request carries no
	// x-compress-base-url header. Required for the "external" instance
	// (fronts whichever provider a0 resolved); ignored in practice for
	// "local" (a0 always sends x-compress-base-url for foundry_slot
	// requests — see routing.go's resolveBackend), but not required to be
	// set, since an unset value only matters if a caller ever omits the
	// header, an operator error this binary should surface as a clear 502,
	// not crash on.
	TargetAPIURL string
	// ProxyToken is accepted and otherwise unused. headroom-ai wrote this
	// value into every instance's env file, but a0 never sends an
	// Authorization header to the local proxy and sends the *provider's*
	// key (not a proxy token) for remote ones (verified during Sprint 3
	// planning — see docs/v5-headroom-replacement.md's proxy binary
	// section) — so there is no real caller of a per-instance shared
	// secret here. Kept only so Provisioner's existing env file (until
	// Sequencing step 3 updates it) doesn't need touching just to add this
	// binary; deliberately not enforced as an auth gate.
	ProxyToken string
	// IntraOpThreads sets onnxruntime's SessionOptions.SetIntraOpNumThreads.
	// Defaults to the value every hand-created headroom-ai instance on
	// ForgeHost already used (internal/compressorctl.kompressIntraThreads).
	IntraOpThreads int

	ModelPath      string
	TokenizerPath  string
	OnnxRuntimeLib string

	Compress compress.Config

	// FailOpenBudgetMS bounds how long one message's compression call is
	// allowed to run before this binary gives up and forwards that
	// message's ORIGINAL content instead — see compressMessage's doc
	// comment for why this is safe (Compress is a pure function with no
	// side effects; an abandoned call just finishes in the background and
	// its result is discarded).
	FailOpenBudgetMS int

	// MaxInflight bounds concurrent compression passes (S3 hardening:
	// deepseek traffic is few-and-huge, so N simultaneous 262K-token
	// requests each holding rawBody + parsed map + re-marshaled body +
	// windowed tokenizer state is the memory balloon shape; the pass — not
	// the relay — is what's bounded). Default 2. Requests beyond the limit
	// WAIT for a slot (tied to their own ctx, so a disconnected client
	// never holds one); they are never rejected.
	MaxInflight int
}

func loadConfig() (config, error) {
	c := config{
		TargetAPIURL:     os.Getenv("OPENAI_TARGET_API_URL"),
		ProxyToken:       os.Getenv("COMPRESS_PROXY_TOKEN"),
		IntraOpThreads:   16,
		Compress:         compress.DefaultConfig(),
		FailOpenBudgetMS: 2000,
	}

	port, err := intEnv("COMPRESS_PORT", 0)
	if err != nil {
		return config{}, err
	}
	if port == 0 {
		return config{}, fmt.Errorf("COMPRESS_PORT is required")
	}
	c.Port = port

	if v, err := intEnv("COMPRESS_ONNX_INTRA_THREADS", c.IntraOpThreads); err != nil {
		return config{}, err
	} else {
		c.IntraOpThreads = v
	}

	c.ModelPath = os.Getenv("FORGE_COMPRESS_MODEL_PATH")
	if c.ModelPath == "" {
		return config{}, fmt.Errorf("FORGE_COMPRESS_MODEL_PATH is required")
	}
	c.TokenizerPath = os.Getenv("FORGE_COMPRESS_TOKENIZER_PATH")
	if c.TokenizerPath == "" {
		return config{}, fmt.Errorf("FORGE_COMPRESS_TOKENIZER_PATH is required")
	}
	c.OnnxRuntimeLib = os.Getenv("FORGE_COMPRESS_ONNXRUNTIME_LIB")
	if c.OnnxRuntimeLib == "" {
		return config{}, fmt.Errorf("FORGE_COMPRESS_ONNXRUNTIME_LIB is required")
	}

	if v, err := intEnv("FORGE_COMPRESS_BYTE_THRESHOLD", c.Compress.ByteThreshold); err != nil {
		return config{}, err
	} else {
		c.Compress.ByteThreshold = v
	}
	if v, err := intEnv("FORGE_COMPRESS_FAILOPEN_BUDGET_MS", c.FailOpenBudgetMS); err != nil {
		return config{}, err
	} else {
		c.FailOpenBudgetMS = v
	}
	if v, err := intEnv("COMPRESS_MAX_INFLIGHT", 2); err != nil {
		return config{}, err
	} else if v < 1 {
		return config{}, fmt.Errorf("COMPRESS_MAX_INFLIGHT must be >= 1, got %d", v)
	} else {
		c.MaxInflight = v
	}

	return c, nil
}

func intEnv(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", name, raw, err)
	}
	return v, nil
}
