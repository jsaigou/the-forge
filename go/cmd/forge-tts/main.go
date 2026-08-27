// Command forge-tts is the pure-Go Qwen3-TTS service that replaces
// scripts/tts_server.py. It serves the OpenAI-compatible speech + voices
// contract and delegates inference to qwentts.cpp (ggml) via its qwen-tts CLI
// or a resident tts-server process.
//
// Env:
//
//	FORGE_TTS_LISTEN          listen addr                  (default 127.0.0.1:8082)
//	FORGE_TTS_BIN_DIR         qwen-tts dir                 (default /opt/forge/qwentts.cpp/build)
//	FORGE_TTS_MODELS_DIR      GGUF dir                     (default /opt/forge/qwentts.cpp/models)
//	FORGE_TTS_SIZE            talker size 0.6b|1.7b        (default 1.7b)
//	FORGE_TTS_BACKEND         GGML backend                 (default Vulkan0)
//	FORGE_TTS_REGISTRY        voices registry dir          (default /opt/forge/tts_voices)
//	FORGE_TTS_INTERNAL_TOKEN  shared secret for /v1/voices*(default "")
//	FORGE_TTS_KOKORO_ENABLED  fast-tier (Kokoro) on/off     (default true)
//	FORGE_TTS_KOKORO_URL      fast-tier (Kokoro) service    (default http://127.0.0.1:8880)
//	FORGE_TTS_KOKORO_TOKEN    optional Bearer token for Kokoro
//	FORGE_TTS_DEFAULT_VOICE   default premium voice id      (default billie)
//	FORGE_TTS_DEFAULT_FAST    default fast voice id         (default af_heart)
//	FORGE_TTS_DISABLED_MODES  comma-separated VoiceMode list refused outright
//	                          (e.g. "voicedesign,base" — default none)
//
// Inference backend selection (FORGE_TTS_INFERENCE, default "cli"):
//
//	"cli"    shell the qwen-tts CLI (model reloaded each call)
//	"server" proxy resident tts-server instances, one per mode:
//	         FORGE_TTS_SERVER_CUSTOM / _DESIGN / _BASE (http://host:port)
//
// Dual-model: the premium engine (above) is fronted by a dualEngine that also
// routes Kokoro-namespaced voices (af_*, am_*, ...) to the fast Kokoro
// service — unless FORGE_TTS_KOKORO_ENABLED=false, in which case no Kokoro
// backend is constructed at all and dualEngine treats it as absent.
//
// As of Sprint 2 (Voice & Speech settings), every var above this line is
// written by the forge daemon's ttsctl.Provisioner into
// /var/lib/forge/tts/forge-tts.env from the tts.engines setting — the
// baked Environment= lines in systemd/forge-tts.service are now only the
// bootstrap defaults for a fresh install, not the operator's live config.
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/tts"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v != "false" && v != "0"
}

func parseDisabledModes(v string) map[tts.VoiceMode]bool {
	out := map[tts.VoiceMode]bool{}
	for _, m := range strings.Split(v, ",") {
		m = strings.TrimSpace(m)
		if m != "" {
			out[tts.VoiceMode(m)] = true
		}
	}
	return out
}

func main() {
	reg := tts.NewRegistry(env("FORGE_TTS_REGISTRY", "/opt/forge/tts_voices"))
	if err := reg.Load(); err != nil {
		log.Printf("warn: registry load: %v", err)
	}

	var backend tts.Backend
	var engineDesc string
	cli := func() tts.Backend {
		return tts.NewCLIBackend(
			env("FORGE_TTS_BIN_DIR", "/opt/forge/qwentts.cpp/build"),
			env("FORGE_TTS_MODELS_DIR", "/opt/forge/qwentts.cpp/models"),
			env("FORGE_TTS_SIZE", "1.7b"),
			env("FORGE_TTS_BACKEND", "Vulkan0"),
			"",
		)
	}
	switch env("FORGE_TTS_INFERENCE", "cli") {
	case "server":
		backend = tts.NewServerBackend(
			env("FORGE_TTS_SERVER_CUSTOM", ""),
			env("FORGE_TTS_SERVER_DESIGN", ""),
			env("FORGE_TTS_SERVER_BASE", ""),
			cli(),
		)
		engineDesc = "tts-server (+cli fallback)"
	default:
		backend = cli()
		engineDesc = "qwen-tts CLI"
	}

	disabled := parseDisabledModes(env("FORGE_TTS_DISABLED_MODES", ""))
	engine := tts.NewQwenTTS(backend, reg, disabled)

	kokoroEnabled := envBool("FORGE_TTS_KOKORO_ENABLED", true)
	kokoroURL := env("FORGE_TTS_KOKORO_URL", "http://127.0.0.1:8880")
	defaultVoice := env("FORGE_TTS_DEFAULT_VOICE", "billie") // Sprint 2 fix: see doc comment
	defaultFast := env("FORGE_TTS_DEFAULT_FAST", "af_heart")

	// dualEngine's concrete type is unexported, so kokoro's construction
	// (also unexported) has to happen entirely inside each branch and be
	// assigned to the exported tts.Engine interface here, rather than
	// declared as a typed nil-or-not variable beforehand.
	var dual tts.Engine
	if kokoroEnabled {
		kokoro := tts.NewKokoroBackend(kokoroURL, os.Getenv("FORGE_TTS_KOKORO_TOKEN"))
		dual = tts.NewDualEngine(engine, kokoro, defaultVoice, defaultFast)
	} else {
		dual = tts.NewDualEngine(engine, nil, defaultVoice, defaultFast)
	}

	srv := tts.NewServer(dual, os.Getenv("FORGE_TTS_INTERNAL_TOKEN"), reg.AudioDir())

	listen := env("FORGE_TTS_LISTEN", "127.0.0.1:8082")
	fastDesc := "disabled"
	if kokoroEnabled {
		fastDesc = "Kokoro " + kokoroURL
	}
	log.Printf("forge-tts listening on %s (inference %s; fast=%s)", listen, engineDesc, fastDesc)
	// Explicit Server instead of http.ListenAndServe so the listener carries
	// header-read/idle timeouts (slowloris posture; synthesis itself can be
	// slow, so no WriteTimeout).
	server := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("forge-tts: %v", err)
	}
}
