package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1 << 20:    "1 MB",
		59339911168 / (1 << 20) * (1 << 20): "55.3 GB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamEventsDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: smith:token\ndata: {\"delta\":\"he\"}\n\n"))
		_, _ = w.Write([]byte("event: status_update\ndata: {\"mode\":\"x\"}\n\n"))
		_, _ = w.Write([]byte(": keepalive\n\n"))
		_, _ = w.Write([]byte("event: smith:message_done\ndata: {}\n\n"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Key: "sk-forge-test", HTTP: srv.Client()}
	var tokens []string
	done := make(chan struct{}, 1)
	err := func() error {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// end the stream after the server finishes writing by cancelling
			<-done
			cancel()
		}()
		return c.StreamEvents(ctx, map[string]func(json.RawMessage){
			"smith:token": func(raw json.RawMessage) { tokens = append(tokens, string(raw)) },
		})
	}()
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("StreamEvents: %v", err)
	}
	close(done)
	if len(tokens) != 1 || !strings.Contains(tokens[0], "\"he\"") {
		t.Fatalf("tokens = %v", tokens)
	}
}

func TestKeyResolutionOrder(t *testing.T) {
	t.Setenv("FORGE_API_KEY", "sk-forge-env")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c.Key != "sk-forge-env" {
		t.Fatalf("Key = %q", c.Key)
	}
}
