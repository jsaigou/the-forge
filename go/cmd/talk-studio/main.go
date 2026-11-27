// Command talk-studio is a browser studio for talking to speech-to-speech models through forge-duplex (ADR-0029). It serves the page, keeps the
// model registry, reference voices, card presets and recorded sessions, and mints the short-lived tickets the page connects with.
//
// Env:
//
//	TALK_STUDIO_LISTEN           listen address (default :8092)
//	TALK_STUDIO_DATA_DIR         data directory (default ./data)
//	TALK_STUDIO_DUPLEX_URL       forge-duplex base URL, e.g. https://duplex.example.ts.net (http://127.0.0.1:8086 for development)
//	TALK_STUDIO_DUPLEX_TOKEN     forge-duplex bearer token (env file, never argv); empty when the service has none
//	TALK_STUDIO_ALLOWED_ORIGINS  extra browser origins allowed to change state, comma-separated
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func main() {
	logger := log.New(os.Stderr, "talk-studio: ", log.LstdFlags|log.LUTC)
	st, err := NewStore(env("TALK_STUDIO_DATA_DIR", "./data"))
	if err != nil {
		logger.Fatalf("data directory: %v", err)
	}
	var origins []string
	for _, o := range strings.Split(os.Getenv("TALK_STUDIO_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	srv := NewServer(Options{Store: st, DuplexURL: env("TALK_STUDIO_DUPLEX_URL", ""), DuplexToken: os.Getenv("TALK_STUDIO_DUPLEX_TOKEN"), VoiceStudioURL: env("TALK_STUDIO_VOICE_STUDIO_URL", ""), AllowedOrigins: origins})
	addr := env("TALK_STUDIO_LISTEN", ":8092")
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Printf("listening on %s (duplex service %q)", addr, env("TALK_STUDIO_DUPLEX_URL", "not configured"))
	logger.Fatal(hs.ListenAndServe())
}
