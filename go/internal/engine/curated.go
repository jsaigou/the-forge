// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"strings"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/launch"
)

// CuratedFigure is one tuple-bound curated memory figure of a config (benchmarks.safe_memory_bytes with its tuple_json, migration 0107).
type CuratedFigure struct {
	Bytes int64
	Tuple string // launch.MemoryTuple JSON
}

// curatedMemory is the fit gate of an engine kind whose footprint is set by its launch tuple (enginekind.Spec.NeedsCuratedMemory; Radiance).
// handled is false for every other kind, which keeps the existing chain (profile, curated, file size). For a handled kind the answer is only ever a
// curated figure whose recorded tuple equals the tuple of the config being loaded: there is no fallback to file size, to a profile, to a model- or
// variant-scoped figure, or to a figure measured at another tuple (a mismatch is a refusal, not a reuse). When several figures match the largest wins.
// When ok is false, why names the tuple that has no figure so the operator knows what to measure and register.
func (m *Manager) curatedMemory(cfg *config.Config, modeName string) (need int64, ok, handled bool, why string) {
	mode, found := cfg.Modes[modeName]
	if !found || len(mode.Services) == 0 {
		return 0, false, false, ""
	}
	svc := mode.Services[0]
	spec, err := svc.Engine()
	if err != nil || !spec.NeedsCuratedMemory {
		return 0, false, false, ""
	}
	want := launch.NewMemoryTuple(svc.BuildID, svc.VariantID, svc.WeightArtifactID, svc.Context, svc.ExtraArgs)
	if mode.ConfigID == 0 || m.d.CuratedFigures == nil {
		return 0, false, true, fmt.Sprintf("%s runs on an engine whose memory depends on its launch tuple and has no catalog config to hold a measured figure; tuple needed: %s", modeName, want.Key())
	}
	var recorded []string
	for _, f := range m.d.CuratedFigures(mode.ConfigID) {
		got, perr := launch.ParseMemoryTuple(f.Tuple)
		if perr != nil {
			m.logf("WARN: fit %s: a recorded memory figure has an unreadable tuple and is ignored: %v", modeName, perr)
			continue
		}
		recorded = append(recorded, got.Key())
		if got.Key() == want.Key() && f.Bytes > need {
			need, ok = f.Bytes, true
		}
	}
	if ok {
		return need, true, true, ""
	}
	if len(recorded) == 0 {
		return 0, false, true, fmt.Sprintf("no measured memory figure is registered for %s; its engine sizes memory from the launch tuple, so file size is not used. Measure this tuple and register it as a safe_memory_bytes benchmark with that tuple: %s", modeName, want.Key())
	}
	return 0, false, true, fmt.Sprintf("the memory figure(s) registered for %s were measured at other tuples and are not reused. Tuple needed: %s. Registered: %s", modeName, want.Key(), strings.Join(recorded, " | "))
}
