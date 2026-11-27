// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBearerTransportOK(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://api.example.com/v1": true, "http://127.0.0.1:9/x": true, "http://localhost/x": true, "http://[::1]/x": true,
		"http://api.example.com/v1": false, "http://8.8.8.8/x": false, "http://[2606:4700::1111]/x": false, "ftp://x/y": false,
		// tailnet and LAN providers legitimately use http (encrypted or local paths)
		"http://100.64.0.1/x": true, "http://10.0.0.1/x": true, "http://192.168.1.5:8090/x": true, "http://core.example.ts.net:8090/x": true, "http://[fd00::1]/x": true,
		"http://evilts.net/x": false, "http://ts.net.evil.example/x": false, "file:///etc/passwd": false,
	} {
		u, _ := url.Parse(raw)
		if got := bearerTransportOK(u); got != want {
			t.Errorf("bearerTransportOK(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestFetchJSONRefusesABearerOverClearTextToARemoteHost(t *testing.T) {
	called := false
	c := roundTripFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })
	_, _, err := fetchJSON(context.Background(), c, "http://api.example.com/credits", "sk-secret", time.Second)
	if err == nil || called {
		t.Fatalf("a bearer over http to a remote host must be refused before any request (err=%v, called=%v)", err, called)
	}
}

func TestFetchJSONStillWorksAgainstALoopbackServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-secret" {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	body, code, err := fetchJSON(context.Background(), newDefaultHTTPClient(time.Second), srv.URL, "sk-secret", time.Second)
	if err != nil || code != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("loopback fetch: %v %d %s", err, code, body)
	}
}

func TestDefaultClientRefusesAHTTPSToHTTPRedirect(t *testing.T) {
	c := newDefaultHTTPClient(time.Second)
	first, _ := http.NewRequest("GET", "https://api.example.com/a", nil)
	down, _ := http.NewRequest("GET", "http://api.example.com/b", nil)
	if err := c.CheckRedirect(down, []*http.Request{first}); err == nil {
		t.Error("an https -> http redirect must be refused")
	}
	same, _ := http.NewRequest("GET", "https://api.example.com/b", nil)
	if err := c.CheckRedirect(same, []*http.Request{first}); err != nil {
		t.Errorf("an https -> https redirect must be allowed: %v", err)
	}
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = first
	}
	if err := c.CheckRedirect(same, via); err == nil {
		t.Error("the redirect chain must be capped")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
