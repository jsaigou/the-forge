// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// recordHistory persists one load attempt's outcome (port of
// engine._record_history): trained context from GGUF metadata (header+KV
// read only), configured vs actual, load time, and a result class —
// "failed", "ctx_exceeds_trained", "ctx_reduced", or "ok". Never fails the
// load: persistence errors are logged and swallowed.
func (m *Manager) recordHistory(modeName string, svc config.Service, actualCtx *int, loadTime time.Duration, failed bool) {
	if m.d.Usage == nil {
		return
	}
	cfg := m.d.Cfg()

	trained := 0
	if svc.Model != "" {
		if meta, err := m.d.ReadMeta(cfg.Paths.ResolveModelPath(svc.Model)); err == nil {
			trained = meta.TrainedCtx
		}
	}

	actual := 0
	if actualCtx != nil {
		actual = *actualCtx
	}
	result := "ok"
	switch {
	case failed:
		result = "failed"
	case trained > 0 && svc.Context > trained:
		result = "ctx_exceeds_trained"
	case actual > 0 && float64(actual) < float64(svc.Context)*0.95:
		result = "ctx_reduced"
	}

	ctx := context.Background()
	if err := m.d.Usage.RecordHistory(ctx, store.ModeHistoryEntry{
		Mode:          modeName,
		TS:            m.d.Now(),
		TrainedCtx:    trained,
		ConfiguredCtx: svc.Context,
		ActualCtx:     actual,
		LoadTimeS:     loadTime.Seconds(),
		Result:        result,
	}); err != nil {
		m.logf("WARN: history recording failed: %v", err)
	}

	kind := "load_ok"
	if failed {
		kind = "load_failed"
	}
	m.recordUsageEvent(kind, modeName, svc.PortRole,
		fmt.Sprintf("result=%s load_time_s=%.1f", result, loadTime.Seconds()))
}

// recordUsageEvent persists a lifecycle event for usage/cost tracking.
// Best-effort — usage tracking must never block a load/unload result.
func (m *Manager) recordUsageEvent(kind, model, slot, detail string) {
	if m.d.Usage == nil {
		return
	}
	if err := m.d.Usage.Record(context.Background(), store.UsageEvent{
		TS:     m.d.Now(),
		Kind:   kind,
		Model:  model,
		Slot:   slot,
		Detail: detail,
	}); err != nil {
		m.logf("WARN: usage event recording failed: %v", err)
	}
}
