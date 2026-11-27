// SPDX-License-Identifier: Apache-2.0

package duplexremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"golang.org/x/net/websocket"
)

// Target names an engine under test and its two test tenants (engine-contract.md
// section 9 and docs/DUPLEX-ENGINE.md section 8). Echo replies in the same
// step; Echo1 replies one step later and recovers the last frame with flush.
// Either may be empty to skip its checks.
type Target struct {
	BaseURL string
	Token   string
	Echo    string
	Echo1   string
	// NoReference skips the checks that send arbitrary bytes as a reference voice.
	NoReference bool
	// Model names a speech-model tenant (not an echo tenant). The model checks run against it:
	// they assert the contract's invariants (lockstep, lengths, ranges, text, determinism,
	// lane release) and never an output value, because a model's audio is stochastic.
	Model string
}

// TestConformanceMockEngine proves the suite against the mock engine, so a
// failure against a real engine is the engine's and not the suite's.
func TestConformanceMockEngine(t *testing.T) {
	a := NewMockEngine(MockEngineOptions{Tenant: "echo", MaxLanes: 2})
	b := NewMockEngine(MockEngineOptions{Tenant: "echo1", DelayFrames: 1, MaxLanes: 2})
	sa, sb := httptest.NewServer(a.Handler()), httptest.NewServer(b.Handler())
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	t.Run("echo", func(t *testing.T) { RunConformance(t, Target{BaseURL: sa.URL, Echo: "echo"}) })
	t.Run("echo1", func(t *testing.T) { RunConformance(t, Target{BaseURL: sb.URL, Echo1: "echo1"}) })
}

// TestConformanceExternalEngine runs the same suite against a real engine. It
// is skipped unless FORGE_DUPLEX_TEST_ENGINE_URL is set; the engine must have
// its test tenants enabled. FORGE_DUPLEX_TEST_ENGINE_TOKEN is read from the
// environment, never from argv.
func TestConformanceExternalEngine(t *testing.T) {
	base := os.Getenv("FORGE_DUPLEX_TEST_ENGINE_URL")
	if base == "" {
		t.Skip("FORGE_DUPLEX_TEST_ENGINE_URL is not set")
	}
	tg := Target{
		BaseURL: base,
		Token:   os.Getenv("FORGE_DUPLEX_TEST_ENGINE_TOKEN"),
		Echo:    envOr("FORGE_DUPLEX_TEST_ECHO", "echo"),
		Echo1:   envOr("FORGE_DUPLEX_TEST_ECHO1", "echo1"),
		// A model tenant, not a test tenant, takes real audio as a reference: the checks that send
		// arbitrary bytes do not apply to it.
		NoReference: os.Getenv("FORGE_DUPLEX_TEST_NO_REFERENCE") == "1",
		Model:       os.Getenv("FORGE_DUPLEX_TEST_MODEL"),
	}
	// "-" names no tenant: an engine with a plugin tenant and no echo1.
	if tg.Echo == "-" {
		tg.Echo = ""
	}
	if tg.Echo1 == "-" {
		tg.Echo1 = ""
	}
	RunConformance(t, tg)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// RunConformance checks an engine against the contract. Each check uses its own
// lane so one failure does not hide the others.
func RunConformance(t *testing.T, tg Target) {
	t.Helper()
	c := &conf{t: t, tg: tg}
	t.Run("info", c.info)
	if tg.Echo != "" {
		t.Run("echo_bit_exact", func(t *testing.T) { c.echoBitExact(t, tg.Echo) })
	}
	if tg.Echo1 != "" {
		t.Run("echo1_delay_and_flush", c.echo1DelayAndFlush)
	}
	name := tg.Echo
	if name == "" {
		name = tg.Echo1
	}
	modelOnly := false
	if tg.Model != "" {
		runModelConformance(t, c, tg.Model)
		if name == "" {
			// The lifecycle and error checks below only need a tenant that opens a lane; a model
			// tenant does, but it cannot take arbitrary bytes as a reference voice.
			name, modelOnly = tg.Model, true
		}
	}
	if !tg.NoReference && !modelOnly {
		t.Run("reference_bytes_accepted", func(t *testing.T) { c.referenceAccepted(t, name) })
		t.Run("large_reference_accepted", func(t *testing.T) { c.largeReference(t, name) })
	}
	t.Run("unknown_tenant", c.unknownTenant)
	t.Run("bad_frame_length_is_fatal", func(t *testing.T) { c.badFrame(t, name) })
	t.Run("bad_sequence_is_fatal", func(t *testing.T) { c.badSequence(t, name) })
	t.Run("first_message_must_be_start", func(t *testing.T) { c.notStart(t, name) })
	t.Run("lane_limit_and_release_on_drop", func(t *testing.T) { c.laneLimit(t, name) })
}

type conf struct {
	t  *testing.T
	tg Target
}

func (c *conf) tenant(name string) *Tenant {
	return New(Config{BaseURL: c.tg.BaseURL, Tenant: name, Token: c.tg.Token, StepTimeout: 2 * time.Second})
}

type infoDoc struct {
	Contract string `json:"contract"`
	Tenants  []struct {
		Name     string `json:"name"`
		MaxLanes int    `json:"max_lanes"`
	} `json:"tenants"`
	Ready             bool  `json:"ready"`
	MaxReferenceBytes int64 `json:"max_reference_bytes"`
}

func (c *conf) getInfo(t *testing.T) infoDoc {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.tg.BaseURL+pathInfo, nil)
	if c.tg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.tg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET info: HTTP %d", resp.StatusCode)
	}
	var d infoDoc
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("info is not JSON: %v", err)
	}
	return d
}

func (c *conf) info(t *testing.T) {
	d := c.getInfo(t)
	if d.Contract != ContractVersion {
		t.Fatalf("contract = %q, want %q", d.Contract, ContractVersion)
	}
	if !d.Ready {
		t.Fatalf("engine reports ready=false; test tenants need no model")
	}
	have := map[string]bool{}
	for _, tn := range d.Tenants {
		have[tn.Name] = true
	}
	for _, want := range []string{c.tg.Echo, c.tg.Echo1} {
		if want != "" && !have[want] {
			t.Fatalf("info lists tenants %v, missing %q (is the test-tenant flag on?)", d.Tenants, want)
		}
	}
	// Load applies the platform's own checks: frame format, tenant present, ready.
	for _, name := range []string{c.tg.Echo, c.tg.Echo1} {
		if name == "" {
			continue
		}
		if _, err := c.tenant(name).Load(context.Background()); err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
	}
}

func (c *conf) echoBitExact(t *testing.T, name string) {
	ctx := context.Background()
	s, err := c.tenant(name).Start(ctx, duplex.TenantConfig{SessionID: "conf-echo", SampleRate: duplex.SampleRate})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	for i := 0; i < 6; i++ {
		in := testFrame(i + 40)
		out, err := s.Step(ctx, in, uint32(i))
		if err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
		sameBits(t, out.Audio, in)
	}
}

func (c *conf) echo1DelayAndFlush(t *testing.T) {
	ctx := context.Background()
	s, err := c.tenant(c.tg.Echo1).Start(ctx, duplex.TenantConfig{SessionID: "conf-echo1", SampleRate: duplex.SampleRate})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	first, err := s.Step(ctx, testFrame(1), 0)
	if err != nil {
		t.Fatalf("Step 0: %v", err)
	}
	if len(first.Audio) != 0 {
		t.Fatalf("first reply of a delay tenant carried audio; it must be flags=0")
	}
	second, err := s.Step(ctx, testFrame(2), 1)
	if err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	sameBits(t, second.Audio, testFrame(1))
	fl, ok := s.(duplex.Flusher)
	if !ok {
		t.Fatalf("session has no Flush")
	}
	last, err := fl.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	sameBits(t, last.Audio, testFrame(2))
	again, err := fl.Flush(ctx)
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(again.Audio) != 0 {
		t.Fatalf("second flush returned audio; nothing was buffered")
	}
}

func (c *conf) referenceAccepted(t *testing.T, name string) {
	ref := make([]byte, 4096)
	for i := range ref {
		ref[i] = byte(i)
	}
	ctx := context.Background()
	s, err := c.tenant(name).Start(ctx, duplex.TenantConfig{
		SessionID: "conf-ref", SampleRate: duplex.SampleRate,
		Card:      map[string]any{"k": "v"},
		Reference: duplex.Reference{ID: "r", Audio: ref, Transcript: "t"},
	})
	if err != nil {
		t.Fatalf("Start with a reference: %v", err)
	}
	defer s.Close()
	if _, err := s.Step(ctx, testFrame(3), 0); err != nil {
		t.Fatalf("Step after a reference: %v", err)
	}
}

func (c *conf) unknownTenant(t *testing.T) {
	_, err := c.tenant("no-such-tenant-xyz").Start(context.Background(), duplex.TenantConfig{SessionID: "x"})
	if err == nil {
		t.Fatalf("start with an unknown tenant succeeded")
	}
	// Load itself reports the absent tenant; a raw start must also be refused.
	raw := c.rawStart(t, "no-such-tenant-xyz", nil)
	defer raw.Close()
	code := c.expectError(t, raw)
	if code != "tenant_unavailable" {
		t.Fatalf("unknown tenant error code = %q, want tenant_unavailable", code)
	}
}

// raw helpers: speak the wire directly so a bad client can be tested.

func (c *conf) dialRaw(t *testing.T) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(c.tg.BaseURL)
	u.Scheme = map[string]string{"http": "ws", "https": "wss"}[u.Scheme]
	u.Path = pathSession
	wc, err := websocket.NewConfig(u.String(), "http://"+u.Host)
	if err != nil {
		t.Fatalf("ws config: %v", err)
	}
	if c.tg.Token != "" {
		wc.Header = http.Header{"Authorization": []string{"Bearer " + c.tg.Token}}
	}
	ws, err := websocket.DialConfig(wc)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = ws.SetDeadline(time.Now().Add(5 * time.Second))
	return ws
}

func (c *conf) rawStart(t *testing.T, tenant string, ref []byte) *websocket.Conn {
	t.Helper()
	ws := c.dialRaw(t)
	body, _ := json.Marshal(map[string]any{
		"type": "start", "contract": ContractVersion, "session_id": "conf-raw", "tenant": tenant,
		"max_minutes": 1, "reference": map[string]any{"bytes": len(ref)},
	})
	if err := sendText(ws, body); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if len(ref) > 0 {
		if err := sendReference(ws, ref); err != nil {
			t.Fatalf("send reference: %v", err)
		}
	}
	return ws
}

func (c *conf) expectReady(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	m, err := recvMsg(ws)
	if err != nil {
		t.Fatalf("waiting for ready: %v", err)
	}
	if !m.Text || !strings.Contains(string(m.Data), `"ready"`) {
		t.Fatalf("expected ready, got text=%v %q", m.Text, truncate(m.Data))
	}
}

// expectError reads one text error and then requires the engine to close.
func (c *conf) expectError(t *testing.T, ws *websocket.Conn) string {
	t.Helper()
	m, err := recvMsg(ws)
	if err != nil {
		t.Fatalf("waiting for error: %v", err)
	}
	if !m.Text {
		t.Fatalf("expected a JSON error, got a binary message of %d bytes", len(m.Data))
	}
	var e struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		Fatal bool   `json:"fatal"`
	}
	if err := json.Unmarshal(m.Data, &e); err != nil || e.Type != "error" {
		t.Fatalf("expected an error message, got %q", truncate(m.Data))
	}
	if !e.Fatal {
		t.Fatalf("error %q was not marked fatal", e.Code)
	}
	if _, err := recvMsg(ws); err == nil {
		t.Fatalf("engine kept the socket open after a fatal error")
	}
	return e.Code
}

func truncate(b []byte) string {
	if len(b) > 120 {
		b = b[:120]
	}
	return string(b)
}

func (c *conf) badFrame(t *testing.T, name string) {
	ws := c.rawStart(t, name, nil)
	defer ws.Close()
	c.expectReady(t, ws)
	if err := sendBinary(ws, make([]byte, 100)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if code := c.expectError(t, ws); code != "bad_frame" {
		t.Fatalf("code = %q, want bad_frame", code)
	}
}

func (c *conf) badSequence(t *testing.T, name string) {
	ws := c.rawStart(t, name, nil)
	defer ws.Close()
	c.expectReady(t, ws)
	if err := sendBinary(ws, encodeStepIn(7, testFrame(1))); err != nil { // first frame must be seq 0
		t.Fatalf("send: %v", err)
	}
	if code := c.expectError(t, ws); code != "bad_sequence" {
		t.Fatalf("code = %q, want bad_sequence", code)
	}
}

func (c *conf) notStart(t *testing.T, name string) {
	ws := c.dialRaw(t)
	defer ws.Close()
	if err := sendBinary(ws, encodeStepIn(0, testFrame(1))); err != nil {
		t.Fatalf("send: %v", err)
	}
	code := c.expectError(t, ws)
	if code != "bad_request" {
		t.Fatalf("code = %q, want bad_request", code)
	}
}

// laneLimit fills the engine's lanes for the tenant, checks the next start is
// refused with session_limit, drops one socket without a close message, and
// checks a lane frees up: the lane's life is the socket's life.
func (c *conf) laneLimit(t *testing.T, name string) {
	max := 0
	for _, tn := range c.getInfo(t).Tenants {
		if tn.Name == name {
			max = tn.MaxLanes
		}
	}
	if max <= 0 {
		t.Skipf("tenant %q advertises no lane limit", name)
	}
	if max > 8 {
		t.Skipf("tenant %q advertises %d lanes; not filling that many", name, max)
	}
	var open []*websocket.Conn
	defer func() {
		for _, ws := range open {
			ws.Close()
		}
	}()
	for i := 0; i < max; i++ {
		ws := c.rawStart(t, name, nil)
		open = append(open, ws)
		c.expectReady(t, ws)
	}
	over := c.rawStart(t, name, nil)
	if code := c.expectError(t, over); code != "session_limit" {
		over.Close()
		t.Fatalf("start beyond max_lanes: code = %q, want session_limit", code)
	}
	over.Close()

	open[0].Close() // abrupt drop, no close message
	open = open[1:]
	deadline := time.Now().Add(3 * time.Second)
	for {
		ws := c.rawStart(t, name, nil)
		m, err := recvMsg(ws)
		if err == nil && m.Text && strings.Contains(string(m.Data), `"ready"`) {
			open = append(open, ws)
			return
		}
		ws.Close()
		if time.Now().After(deadline) {
			t.Fatalf("lane was not released within 3 s of the socket dropping")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// largeReference sends a reference bigger than an ordinary step queue allows (a
// 3-minute clip can be tens of MB) and checks the engine accepts it when it
// advertises room. Repeated, because the engine decides how big the next
// message may be only after it has read the start, and a race there shows up as
// an occasional refusal, not a constant one.
func (c *conf) largeReference(t *testing.T, name string) {
	const size = 17 << 20 // far above one WebSocket message; sent as 1 MiB chunks
	if c.getInfo(t).MaxReferenceBytes < size {
		t.Skipf("engine advertises no room for a %d MiB reference", size>>20)
	}
	ref := make([]byte, size)
	for i := range ref {
		ref[i] = byte(i * 31)
	}
	rounds := 3
	if v := os.Getenv("FORGE_DUPLEX_TEST_LARGE_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rounds = n
		}
	}
	for i := 0; i < rounds; i++ {
		ws := c.rawStart(t, name, ref)
		_ = ws.SetDeadline(time.Now().Add(60 * time.Second))
		c.expectReady(t, ws)
		ws.Close()
	}
}
