// SPDX-License-Identifier: Apache-2.0

package netguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckURL(t *testing.T) {
	for u, wantErr := range map[string]bool{
		"https://api.frankfurter.app/latest?from=USD": false, "http://8.8.8.8/x": false, "HTTPS://example.com/": false,
		"http://169.254.169.254/latest/meta-data/": true, "http://127.0.0.1:8095/": true, "http://100.64.0.1/": true, "http://[64:ff9b::7f00:1]/": true,
		"http://[::1]/": true, "http://user:pw@example.com/": true, "ftp://example.com/": true, "file:///etc/passwd": true, "javascript:alert(1)": true,
		"//example.com/": true, "http:///nohost": true, "": true,
	} {
		if err := CheckURL(u); (err != nil) != wantErr {
			t.Errorf("CheckURL(%q) err=%v, wantErr=%v", u, err, wantErr)
		}
	}
}

func TestClientRefusesLoopbackAndAllowOverridePermitsIt(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	c := NewClient(nil)
	c.Timeout = 3 * time.Second
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("a loopback fetch must be refused")
	}
	if hits != 0 {
		t.Fatal("the loopback server was contacted")
	}
	ca := NewClient(func(h string) bool { return h == "127.0.0.1" })
	if resp, err := ca.Get(srv.URL); err != nil {
		t.Fatalf("allowed host: %v", err)
	} else {
		resp.Body.Close()
	}
}

// A permitted server that redirects to an internal address must not be followed there: the redirect hop dials through the same guard.
func TestClientRefusesRedirectHopToInternalAddress(t *testing.T) {
	internal := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { internal++ }))
	t.Cleanup(target.Close)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	t.Cleanup(front.Close)
	dials := 0
	c := NewClient(func(string) bool { dials++; return dials == 1 }) // approve only the first dial (the front); the redirect hop is judged normally
	c.Timeout = 3 * time.Second
	if resp, err := c.Get(front.URL); err == nil {
		resp.Body.Close()
		t.Fatal("the redirect to a loopback target must fail")
	}
	if internal != 0 {
		t.Fatalf("the internal target was contacted %d time(s)", internal)
	}
}
