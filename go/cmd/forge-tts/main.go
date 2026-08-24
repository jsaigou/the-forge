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
//	FORGE_TTS_KOKORO_URL      fast-tier (Kokoro) service    (default http://127.0.0.1:8880)
//	FORGE_TTS_KOKORO_TOKEN    optional Bearer token for Kokoro
//	FORGE_TTS_DEFAULT_VOICE   default premium voice id      (default billie)
//	FORGE_TTS_DEFAULT_FAST    default fast voice id         (default af_heart)
//
// Inference backend selection (FORGE_TTS_INFERENCE, default "cli"):
//
//	"cli"    shell the qwen-tts CLI (model reloaded each call)
//	"server" proxy resident tts-server instances, one per mode:
//	         FORGE_TTS_SERVER_CUSTOM / _DESIGN / _BASE (http://host:port)
//
// Dual-model: the premium engine (above) is fronted by a dualEngine that also
// routes Kokoro-namespaced voices (af_*, am_*, ...) to the fast Kokoro service.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/jsaigou/the-forge/internal/tts"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

	engine := tts.NewQwenTTS(backend, reg)

	kokoro := tts.NewKokoroBackend(
		env("FORGE_TTS_KOKORO_URL", "http://127.0.0.1:8880"),
		os.Getenv("FORGE_TTS_KOKORO_TOKEN"),
	)
	dual := tts.NewDualEngine(
		engine,
		kokoro,
		env("FORGE_TTS_DEFAULT_VOICE", "af_heart"),
		env("FORGE_TTS_DEFAULT_FAST", "af_heart"),
	)

	srv := tts.NewServer(dual, os.Getenv("FORGE_TTS_INTERNAL_TOKEN"), reg.AudioDir())

	listen := env("FORGE_TTS_LISTEN", "127.0.0.1:8082")
	log.Printf("forge-tts listening on %s (inference %s; fast=Kokoro %s)", listen, engineDesc, env("FORGE_TTS_KOKORO_URL", "http://127.0.0.1:8880"))
	if err := http.ListenAndServe(listen, srv.Handler()); err != nil {
		log.Fatalf("forge-tts: %v", err)
	}
}
