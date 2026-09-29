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
		0:                                   "0 B",
		512:                                 "512 B",
		1 << 20:                             "1 MB",
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

// TestAPIErrorMessage covers the three-tier fallback (Phase 4, multilanguage
// plan): a mirrored code -> the translated errors.* catalog entry; a
// known-shape body with no mirrored code -> the server's own (always-
// English) error field, not the raw JSON blob; anything that isn't even
// valid JSON -> the raw (truncated) body text, same as before this change.
func TestAPIErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "mirrored code uses translated catalog entry",
			body: `{"error":"You need to sign in to do this.","code":"auth_required"}`,
			want: "You need to sign in to do this.",
		},
		{
			name: "code with params interpolates",
			body: `{"error":"config not found.","code":"not_found","params":{"resource":"config"}}`,
			want: "config not found.",
		},
		{
			name: "unmirrored code falls back to the raw error field",
			body: `{"error":"some future backend-only message","code":"some_new_code_not_yet_mirrored"}`,
			want: "some future backend-only message",
		},
		{
			name: "no code, has error field",
			body: `{"error":"plain server error"}`,
			want: "plain server error",
		},
		{
			name: "not JSON at all falls back to raw truncated body",
			body: "<html>502 Bad Gateway</html>",
			want: "<html>502 Bad Gateway</html>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apiErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("apiErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
