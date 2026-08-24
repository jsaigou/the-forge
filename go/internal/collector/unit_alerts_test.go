// SPDX-License-Identifier: Apache-2.0

package collector

import "testing"

// newTestCollectorForAlerts builds a bare Collector sufficient for
// unitAlerts, which only touches c.lastNRestarts — bypassing New()'s full
// Options requirement (Cfg/Systemd/GPU) since this test targets one pure
// method, not a full probe cycle.
func newTestCollectorForAlerts() *Collector {
	return &Collector{lastNRestarts: map[string]uint32{}}
}

func TestUnitAlertsOOM(t *testing.T) {
	c := newTestCollectorForAlerts()
	units := map[string]UnitState{
		"ai-mode-comfyui": {ActiveState: "failed", Result: "oom-kill"},
	}
	alerts := c.unitAlerts(units)
	if len(alerts) != 1 || alerts[0].Code != "UNIT_OOM" || alerts[0].Unit != "ai-mode-comfyui" {
		t.Fatalf("expected one UNIT_OOM alert for ai-mode-comfyui, got %+v", alerts)
	}
}

func TestUnitAlertsCrash(t *testing.T) {
	c := newTestCollectorForAlerts()
	for _, result := range []string{"core-dump", "signal", "watchdog", "exit-code"} {
		units := map[string]UnitState{"forge-tts": {ActiveState: "failed", Result: result, ExecMainStatus: 139}}
		alerts := c.unitAlerts(units)
		if len(alerts) != 1 || alerts[0].Code != "UNIT_CRASH" {
			t.Errorf("Result=%q: expected one UNIT_CRASH alert, got %+v", result, alerts)
		}
	}
}

func TestUnitAlertsNoAlertOnSuccess(t *testing.T) {
	c := newTestCollectorForAlerts()
	units := map[string]UnitState{"forge-a1": {ActiveState: "active", Result: "success"}}
	alerts := c.unitAlerts(units)
	if len(alerts) != 0 {
		t.Fatalf("expected no alerts for a healthy active unit, got %+v", alerts)
	}
}

// TestUnitAlertsRestartBaseline proves a unit's first sighting only records
// an NRestarts baseline and does NOT emit UNIT_RESTARTED — a long-running
// unit can easily already have NRestarts > 0 from before forge itself
// last started, which must not read as a new event.
func TestUnitAlertsRestartBaseline(t *testing.T) {
	c := newTestCollectorForAlerts()
	units := map[string]UnitState{"forge-a1": {ActiveState: "active", NRestarts: 3}}
	if alerts := c.unitAlerts(units); len(alerts) != 0 {
		t.Fatalf("expected no alert on first sighting (baseline only), got %+v", alerts)
	}
	if c.lastNRestarts["forge-a1"] != 3 {
		t.Fatalf("expected baseline 3 recorded, got %d", c.lastNRestarts["forge-a1"])
	}

	// A later cycle with an unchanged NRestarts still shouldn't alert.
	if alerts := c.unitAlerts(units); len(alerts) != 0 {
		t.Fatalf("expected no alert when NRestarts unchanged, got %+v", alerts)
	}

	// NRestarts increasing IS a new restart.
	units2 := map[string]UnitState{"forge-a1": {ActiveState: "active", NRestarts: 4}}
	alerts := c.unitAlerts(units2)
	if len(alerts) != 1 || alerts[0].Code != "UNIT_RESTARTED" {
		t.Fatalf("expected one UNIT_RESTARTED alert, got %+v", alerts)
	}
}

func TestUnitAlertsForgetsVanishedUnits(t *testing.T) {
	c := newTestCollectorForAlerts()
	c.unitAlerts(map[string]UnitState{"forge-old": {NRestarts: 5}})
	if _, ok := c.lastNRestarts["forge-old"]; !ok {
		t.Fatalf("expected baseline recorded")
	}
	// The unit vanishes from a later cycle's probe set (config changed).
	c.unitAlerts(map[string]UnitState{"forge-new": {NRestarts: 0}})
	if _, ok := c.lastNRestarts["forge-old"]; ok {
		t.Fatalf("expected stale baseline for vanished unit to be dropped")
	}
}

func TestUnitAlertsSorted(t *testing.T) {
	c := newTestCollectorForAlerts()
	units := map[string]UnitState{
		"z-unit": {Result: "oom-kill"},
		"a-unit": {Result: "oom-kill"},
	}
	alerts := c.unitAlerts(units)
	if len(alerts) != 2 || alerts[0].Unit != "a-unit" || alerts[1].Unit != "z-unit" {
		t.Fatalf("expected alerts sorted by (code, unit), got %+v", alerts)
	}
}
