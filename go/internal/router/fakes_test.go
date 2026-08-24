// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeCatalog is a SlotCatalog that returns canned probe results per port.
// Used in tests to simulate healthy/unhealthy/busy slots without real HTTP.
type fakeCatalog struct {
	mu       sync.Mutex
	probes   map[int]SlotProbe
	busy     map[int]bool
	calls    int // counts Probe() calls — for TTL cache verification
	busyCals int
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{
		probes: make(map[int]SlotProbe),
		busy:   make(map[int]bool),
	}
}

func (f *fakeCatalog) setProbe(port int, p SlotProbe) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes[port] = p
}

func (f *fakeCatalog) setBusy(port int, b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy[port] = b
}

func (f *fakeCatalog) Probe(port int, _ time.Duration) SlotProbe {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.probes[port]
}

func (f *fakeCatalog) IsBusy(port int, _ time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busyCals++
	return f.busy[port]
}

// fakeCompressor is an in-memory store.Routing for tests. Only Proxies() and
// Providers() are exercised by the router; the rest panic to keep tests honest.
type fakeCompressor struct {
	proxies   []store.ProxyRow
	providers []store.ProviderRow
}

func (f *fakeCompressor) SaveProxy(context.Context, store.ProxyRow) error       { panic("unused") }
func (f *fakeCompressor) DeleteProxy(context.Context, string) error             { panic("unused") }
func (f *fakeCompressor) Proxies(context.Context) ([]store.ProxyRow, error)     { return f.proxies, nil }
func (f *fakeCompressor) SaveProvider(context.Context, store.ProviderRow) error { panic("unused") }
func (f *fakeCompressor) DeleteProvider(context.Context, int64) error           { panic("unused") }
func (f *fakeCompressor) Providers(context.Context) ([]store.ProviderRow, error) {
	return f.providers, nil
}
func (f *fakeCompressor) ProviderByID(_ context.Context, id int64) (store.ProviderRow, bool, error) {
	for _, p := range f.providers {
		if p.ID == id {
			return p, true, nil
		}
	}
	return store.ProviderRow{}, false, nil
}
func (f *fakeCompressor) ProviderByName(_ context.Context, name string) (store.ProviderRow, bool, error) {
	for _, p := range f.providers {
		if p.Name == name {
			return p, true, nil
		}
	}
	return store.ProviderRow{}, false, nil
}
func (f *fakeCompressor) LinkProxyToProvider(context.Context, int64, *int64) error { panic("unused") }
func (f *fakeCompressor) RecordSavings(context.Context, int64, time.Time, int64, int64) error {
	panic("unused")
}
func (f *fakeCompressor) Savings(context.Context, time.Time) (map[string]store.SavingsTotal, error) {
	panic("unused")
}
func (f *fakeCompressor) RecordSavingsSample(context.Context, store.CompressorSavingsSampleRow, []store.CompressorLabelSample) error {
	panic("unused")
}
func (f *fakeCompressor) SavingsSummary(context.Context, time.Time) (map[string]store.CompressorProxySummary, error) {
	panic("unused")
}

// fakeSettings is an in-memory store.Settings.
type fakeSettings struct {
	mu sync.Mutex
	kv map[string][]byte
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{kv: make(map[string][]byte)}
}

func (f *fakeSettings) set(key string, val []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = val
}

func (f *fakeSettings) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.kv[key]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeSettings) Set(_ context.Context, key string, val []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = val
	return nil
}

// fakeAudit is an in-memory store.Audit that records entries for inspection.
type fakeAudit struct {
	mu      sync.Mutex
	entries []store.AuditEntry
}

func (f *fakeAudit) Write(_ context.Context, e store.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

// List satisfies store.Audit (Sprint C added it); this fake is only ever
// exercised via Write/last in router tests, so it's an unfiltered stub.
func (f *fakeAudit) List(_ context.Context, _, _ string, _ int) ([]store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.AuditEntry(nil), f.entries...), nil
}

func (f *fakeAudit) last() (store.AuditEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return store.AuditEntry{}, false
	}
	return f.entries[len(f.entries)-1], true
}
