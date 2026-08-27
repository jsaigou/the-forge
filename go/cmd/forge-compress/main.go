// SPDX-License-Identifier: Apache-2.0

// forge-compress is the Sprint 3 replacement for headroom-ai
// (docs/v5-headroom-replacement.md): an OpenAI-compatible reverse proxy
// that compresses oversized chat message content via the real
// chopratejas/kompress-v2-base ONNX model + ModernBERT tokenizer
// (internal/compress, internal/onnxscorer, internal/hftokenizer), then
// forwards to whichever upstream the caller specifies.
//
// One binary, two deployed roles (docs/v5-headroom-replacement.md Sprint
// 3's Architecture section): a shared "local" instance fronting all A1-A4
// slots (upstream chosen per-request via the x-compress-base-url header —
// see server.go's resolveUpstream), and a shared "external" instance
// fronting whichever remote provider is configured via
// OPENAI_TARGET_API_URL. Deliberately built natively wherever it runs
// (not cross-compiled) — see the plan's "Build & deploy" section — since
// it links real native libtokenizers/libonnxruntime via cgo; forge's
// own build never imports this package or internal/compress transitively,
// so that binary's pure-Go cross-compile is completely unaffected by this
// one's native dependency.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jsaigou/the-forge/internal/compress"
	"github.com/jsaigou/the-forge/internal/hftokenizer"
	"github.com/jsaigou/the-forge/internal/onnxscorer"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("forge-compress: %v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.ProxyToken != "" {
		log.Printf("forge-compress: COMPRESS_PROXY_TOKEN is set but unused by this binary — a0 never sends a matching Authorization header to this proxy (verified during Sprint 3 planning); see config.go's ProxyToken doc comment")
	}

	tok, err := hftokenizer.New(cfg.TokenizerPath)
	if err != nil {
		return err
	}
	defer tok.Close()

	scorer, err := onnxscorer.New(cfg.ModelPath, cfg.OnnxRuntimeLib, cfg.IntraOpThreads)
	if err != nil {
		return err
	}
	defer scorer.Close()

	engine := &compress.Engine{Tokenizer: tok, Scorer: scorer, Config: cfg.Compress}
	m := newMetrics()
	srv := newServer(cfg, engine, m)

	// Loopback bind only — same SSRF posture every other Compressor proxy on
	// this host relies on (internal/router/proxy.go's comment: "the
	// loopback bind ... is the only reason this isn't exploitable").
	addr := "127.0.0.1:" + strconv.Itoa(cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// Header-read/idle timeouts for slowloris posture; no WriteTimeout —
	// compression responses stream for as long as the upstream generation runs.
	httpServer := &http.Server{
		Handler:           srv.mux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("forge-compress: listening on %s (model=%s tokenizer=%s target=%q)", addr, cfg.ModelPath, cfg.TokenizerPath, cfg.TargetAPIURL)
		errCh <- httpServer.Serve(ln)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
