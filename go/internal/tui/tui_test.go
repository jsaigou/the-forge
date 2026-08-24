package tui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jsaigou/the-forge/internal/cli"
)

// TestTUIBootsRendersQuits drives a real bubbletea program against a stub
// API: Init fires the overview fetch, the first frame renders, then "q" quits.
func TestTUIBootsRendersQuits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mode":"gemma4-e4b-qat","hostname":"stub","version":"v0.5.0-test",
			"slots":{"a1":"gemma4-e4b-qat","a2":null,"a3":null,"a4":null},
			"slot_labels":{"a1":"A1"},"modes_available":{},"service_modes":{},
			"switch":{"in_progress":false},"slot_loading":{},"slot_unloading":{}}`))
	})
	mux.HandleFunc("GET /api/v1/scheduler/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slots":{"a1":"gemma4-e4b-qat"},"slot_labels":{},"idle_seconds":{"a1":5},
			"slot_memory_bytes":{"a1":7800000000},"unit_memory_bytes":{},
			"memory_budget":{"total_bytes":128849018880,"used_bytes":6400000000,"free_bytes":122449018880},
			"queue":[]}`))
	})
	mux.HandleFunc("GET /api/v1/metrics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mode":"x","memory":{},"cpu":{},"gtt_used_bytes":6400000000,"gtt_total_bytes":128849018880}`))
	})
	mux.HandleFunc("GET /api/v1/notifications", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"notifications":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &cli.Client{BaseURL: srv.URL, Key: "sk-forge-test", HTTP: srv.Client()}
	s := New(c)
	p := tea.NewProgram(s,
		tea.WithInput(strings.NewReader("q")),
		tea.WithOutput(io.Discard),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		t.Fatalf("program: %v", err)
	}
}
