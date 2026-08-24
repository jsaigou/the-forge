// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeAudit records audit writes for asserting auto-recovery's audit trail.
type fakeAudit struct {
	entries []store.AuditEntry
}

func (a *fakeAudit) Write(_ context.Context, e store.AuditEntry) error {
	a.entries = append(a.entries, e)
	return nil
}

func (a *fakeAudit) List(_ context.Context, _, _ string, _ int) ([]store.AuditEntry, error) {
	return nil, nil
}

var _ store.Audit = (*fakeAudit)(nil)

// deviceLostSnap returns a snapshot where slot a1 (port 8080) is loaded with
// mode "qwen38-27b", with a real router error storm on that slot and no
// in-flight requests — the state the confirmation + idle gates require to
// let auto-recovery proceed.
func deviceLostSnap() *collector.Snapshot {
	snap := snapWith(collector.Metrics{})
	snap.Slots["a1"] = collector.SlotState{Slot: "a1", Port: 8080, Mode: "qwen38-27b"}
	snap.Inference["a1"] = collector.SlotInference{SlotErrors: 5}
	return snap
}

func kernelRingTimeout() func(context.Context, int, time.Time) ([]string, error) {
	return func(_ context.Context, n int, _ time.Time) ([]string, error) {
		return []string{"amdgpu: ring comp_1.2.0 timeout, signaled seq=1, emitted seq=4"}, nil
	}
}

func TestMaybeAutoRecover(t *testing.T) {
	t.Run("journal-confirmed device-lost reloads same mode", func(t *testing.T) {
		snap := deviceLostSnap()
		placer := &stubPlacer{}
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
		})
		s.maybeAutoRecover(context.Background(), "SLOT_ERROR_STORM", "8080")

		if len(placer.unloads) != 1 || placer.unloads[0] != "a1" {
			t.Errorf("unloads = %v, want [a1]", placer.unloads)
		}
		if len(placer.loads) != 1 || placer.loads[0] != (loadCall{Mode: "qwen38-27b", Slot: "a1"}) {
			t.Errorf("loads = %v, want [{qwen38-27b a1}]", placer.loads)
		}
	})

	t.Run("no journal signature = no auto-reload", func(t *testing.T) {
		snap := deviceLostSnap()
		placer := &stubPlacer{}
		s := New(Deps{
			Source: collector.NewStatic(snap),
			Now:    time.Now,
			Logf:   func(string, ...any) {},
			Placer: placer,
			KernelJournal: func(_ context.Context, n int, _ time.Time) ([]string, error) {
				return []string{"amdgpu: normal boot"}, nil
			},
		})
		s.maybeAutoRecover(context.Background(), "SLOT_ERROR_STORM", "8080")

		if len(placer.unloads) != 0 || len(placer.loads) != 0 {
			t.Errorf("auto-recovered without journal signature: unloads=%v loads=%v", placer.unloads, placer.loads)
		}
	})

	t.Run("journal signature without a current router error storm = no auto-reload", func(t *testing.T) {
		snap := deviceLostSnap()
		snap.Inference["a1"] = collector.SlotInference{SlotErrors: 0} // storm has already cleared
		placer := &stubPlacer{}
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
		})
		s.maybeAutoRecover(context.Background(), "SLOT_ERROR_STORM", "8080")

		if len(placer.unloads) != 0 || len(placer.loads) != 0 {
			t.Errorf("auto-recovered without a current error storm: unloads=%v loads=%v", placer.unloads, placer.loads)
		}
	})

	t.Run("busy slot (in-flight requests) never auto-recovered, escalates instead", func(t *testing.T) {
		snap := deviceLostSnap()
		snap.Inference["a1"] = collector.SlotInference{SlotErrors: 5, RequestsProcessing: 1}
		placer := &stubPlacer{}
		db := openDB(t)
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Store:         db,
			Settings:      db.Settings(),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
		})
		ctx := context.Background()
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")

		if len(placer.unloads) != 0 || len(placer.loads) != 0 {
			t.Errorf("auto-recovered a busy slot: unloads=%v loads=%v", placer.unloads, placer.loads)
		}
		acts, err := s.ListActions(ctx, "", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		foundEscalation := false
		for _, a := range acts {
			if a.Kind == KindRunbook && a.CreatedBy == "smith" {
				foundEscalation = true
			}
		}
		if !foundEscalation {
			t.Errorf("expected an escalation runbook for the contradictory busy+device-lost state, got %+v", acts)
		}
	})

	t.Run("a correct bail-out does not consume the cooldown", func(t *testing.T) {
		// First call: no journal signature, bails out early. Second call: a
		// real confirmed device-lost. Both must be evaluated on their own
		// merits — the first bail-out must not have stamped the cooldown
		// and blocked the second, genuine recovery (found live 2026-08-18:
		// the old code stamped the cooldown before the confirmation gate
		// ran at all).
		snap := deviceLostSnap()
		placer := &stubPlacer{}
		var confirmed bool
		s := New(Deps{
			Source: collector.NewStatic(snap),
			Now:    time.Now,
			Logf:   func(string, ...any) {},
			Placer: placer,
			KernelJournal: func(_ context.Context, n int, _ time.Time) ([]string, error) {
				if confirmed {
					return []string{"amdgpu: ring comp_1.2.0 timeout, signaled seq=1, emitted seq=4"}, nil
				}
				return []string{"amdgpu: normal boot"}, nil
			},
		})
		ctx := context.Background()
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")
		if len(placer.unloads) != 0 {
			t.Fatalf("first call (no signature) should not have recovered: unloads=%v", placer.unloads)
		}

		confirmed = true
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")
		if len(placer.unloads) != 1 || placer.unloads[0] != "a1" {
			t.Errorf("second call (confirmed) should have recovered: unloads=%v, want [a1] (cooldown must not have been burned by the first bail-out)", placer.unloads)
		}
	})

	t.Run("disabled setting = no auto-reload", func(t *testing.T) {
		db := openDB(t)
		if err := db.Settings().Set(context.Background(), SettingAutoRecoverDeviceLost, []byte("false")); err != nil {
			t.Fatal(err)
		}
		snap := deviceLostSnap()
		placer := &stubPlacer{}
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Store:         db,
			Settings:      db.Settings(),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
		})
		s.maybeAutoRecover(context.Background(), "SLOT_ERROR_STORM", "8080")

		if len(placer.unloads) != 0 || len(placer.loads) != 0 {
			t.Errorf("auto-recovered despite disabled setting: unloads=%v loads=%v", placer.unloads, placer.loads)
		}
	})

	t.Run("recurrence within cooldown escalates instead of reloading", func(t *testing.T) {
		snap := deviceLostSnap()
		placer := &stubPlacer{}
		db := openDB(t)
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Store:         db,
			Settings:      db.Settings(),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
		})
		ctx := context.Background()
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")
		// Immediately recur: should NOT reload again (cooldown), and should
		// propose an escalation runbook.
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")

		if len(placer.unloads) != 1 {
			t.Errorf("unloads = %v, want exactly 1 (cooldown blocks the 2nd)", placer.unloads)
		}
		acts, err := s.ListActions(ctx, "", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		foundEscalation := false
		for _, a := range acts {
			if a.Kind == KindRunbook && a.CreatedBy == "smith" {
				foundEscalation = true
			}
		}
		if !foundEscalation {
			t.Errorf("expected a smith-created runbook escalation action, got %+v", acts)
		}
	})

	t.Run("reload failure escalates", func(t *testing.T) {
		snap := deviceLostSnap()
		fail := engine.Result{Success: false, Message: "reload failed: OOM"}
		placer := &stubPlacer{loadResult: &fail}
		db := openDB(t)
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Store:         db,
			Settings:      db.Settings(),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
		})
		ctx := context.Background()
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")

		acts, err := s.ListActions(ctx, "", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		foundEscalation := false
		for _, a := range acts {
			if a.Kind == KindRunbook && a.CreatedBy == "smith" {
				foundEscalation = true
			}
		}
		if !foundEscalation {
			t.Errorf("expected escalation runbook after reload failure, got %+v", acts)
		}
	})

	t.Run("crash loop hands off to cloud offering", func(t *testing.T) {
		snap := deviceLostSnap()
		fail := engine.Result{Success: false, Message: "reload failed: OOM"}
		placer := &stubPlacer{loadResult: &fail}
		db := openDB(t)
		seedBrainCatalog(t, db) // creates a "deepseek-chat" remote offering
		setSetting(t, db, SettingModel, `"qwen38-27b"`)
		setSetting(t, db, SettingHandoffOfferings, `[1]`) // deepseek-chat is offering id 1 in the fixture
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Store:         db,
			Settings:      db.Settings(),
			Catalog:       db.Catalog(),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
			ProviderHealth: func(_ context.Context, _ string) (string, error) {
				return "reachable", nil
			},
		})
		ctx := context.Background()
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")

		// The brain must have been swapped to the cloud offering.
		got := s.settingModel(ctx)
		if got != "deepseek-chat" {
			t.Errorf("smith.model = %q, want %q (cloud handoff)", got, "deepseek-chat")
		}
		// And an investigation recording the handoff must exist.
		invs, err := s.ListInvestigations(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		foundHandoff := false
		for _, inv := range invs {
			if inv.Trigger == "anomaly:INFERENCE_DEVICE_LOST_HANDOFF" {
				foundHandoff = true
			}
		}
		if !foundHandoff {
			t.Errorf("expected INFERENCE_DEVICE_LOST_HANDOFF investigation, got %+v", invs)
		}
		// No runbook escalation should be needed (handoff succeeded).
		acts, err := s.ListActions(ctx, "", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range acts {
			if a.Kind == KindRunbook && a.CreatedBy == "smith" {
				t.Errorf("unexpected runbook escalation after successful handoff: %+v", a)
			}
		}
	})

	t.Run("auto_handoff_cloud=false forces runbook, no swap", func(t *testing.T) {
		snap := deviceLostSnap()
		fail := engine.Result{Success: false, Message: "reload failed: OOM"}
		placer := &stubPlacer{loadResult: &fail}
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"qwen38-27b"`)
		setSetting(t, db, SettingHandoffOfferings, `[1]`)
		setSetting(t, db, SettingAutoHandoffCloud, `false`)
		s := New(Deps{
			Source:        collector.NewStatic(snap),
			Store:         db,
			Settings:      db.Settings(),
			Catalog:       db.Catalog(),
			Now:           time.Now,
			Logf:          func(string, ...any) {},
			Placer:        placer,
			KernelJournal: kernelRingTimeout(),
			ProviderHealth: func(_ context.Context, _ string) (string, error) {
				return "reachable", nil
			},
		})
		ctx := context.Background()
		s.maybeAutoRecover(ctx, "SLOT_ERROR_STORM", "8080")

		if got := s.settingModel(ctx); got != "qwen38-27b" {
			t.Errorf("smith.model = %q, want %q (handoff disabled)", got, "qwen38-27b")
		}
		acts, err := s.ListActions(ctx, "", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		foundEscalation := false
		for _, a := range acts {
			if a.Kind == KindRunbook && a.CreatedBy == "smith" {
				foundEscalation = true
			}
		}
		if !foundEscalation {
			t.Errorf("expected runbook escalation when handoff disabled, got %+v", acts)
		}
	})
}
