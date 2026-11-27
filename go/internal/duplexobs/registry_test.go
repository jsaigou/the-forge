// SPDX-License-Identifier: Apache-2.0

package duplexobs

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

func countMod(n, m int) int {
	c := 0
	for i := 0; i < n; i++ {
		if i%m == 0 {
			c++
		}
	}
	return c
}

func TestConcurrentSessionsAreRaceSafeAndAggregate(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	reg := NewRegistry(Config{Now: func() time.Time { return fixed }})

	const (
		sessions   = 8
		workers    = 4
		iterations = 200
	)
	obs := make([]*Obs, sessions)
	ids := make([]string, sessions)
	for s := 0; s < sessions; s++ {
		ids[s] = fmt.Sprintf("dup_%02d", s)
		obs[s] = reg.Start(duplex.SessionInfo{SessionID: ids[s], Tenant: "loopback"})
	}
	if got := reg.Len(); got != sessions {
		t.Fatalf("registry len = %d, want %d", got, sessions)
	}

	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(o *Obs, worker int) {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					o.Input(i%7 == 0)
					o.Step(time.Duration(worker*iterations+i+1) * time.Millisecond)
					o.Output(time.Duration(i+1)*time.Millisecond, i%11 == 0)
					o.Queues(worker%3, i%5)
					if i%50 == 0 {
						o.Drop("queue_full")
					}
					if i%100 == 0 {
						o.Error(duplex.ErrInternal)
					}
					if i%10 == 0 {
						o.Observe(duplex.Event{Kind: duplex.EventText})
					}
				}
			}(obs[s], w)
		}
	}
	wg.Wait()

	perSession := struct {
		in, lateIn, lateOut, drops, errors, text int
	}{
		in:      workers * iterations,
		lateIn:  workers * countMod(iterations, 7),
		lateOut: workers * countMod(iterations, 11),
		drops:   workers * countMod(iterations, 50),
		errors:  workers * countMod(iterations, 100),
		text:    workers * countMod(iterations, 10),
	}
	for i, o := range obs {
		s := o.Summary()
		if s.InFrames != uint64(perSession.in) || s.OutFrames != uint64(perSession.in) {
			t.Errorf("%s frames = %d/%d, want %d", ids[i], s.InFrames, s.OutFrames, perSession.in)
		}
		if s.LateInputs != uint64(perSession.lateIn) || s.LateOutputs != uint64(perSession.lateOut) {
			t.Errorf("%s late = %d/%d, want %d/%d", ids[i], s.LateInputs, s.LateOutputs,
				perSession.lateIn, perSession.lateOut)
		}
		if s.Dropped != uint64(perSession.drops) || s.TextDeltas != uint64(perSession.text) {
			t.Errorf("%s drops/text = %d/%d, want %d/%d", ids[i], s.Dropped, s.TextDeltas,
				perSession.drops, perSession.text)
		}
		if s.ErrorsByCode[duplex.ErrInternal] != uint64(perSession.errors) {
			t.Errorf("%s errors = %+v, want %d internal", ids[i], s.ErrorsByCode, perSession.errors)
		}
	}

	for i, id := range ids {
		s, ok := reg.Close(id, "client_done")
		if !ok {
			t.Fatalf("%s close returned not found", id)
		}
		if !s.Closed || s.CloseReason != "client_done" {
			t.Errorf("%s close summary = %+v", ids[i], s)
		}
	}
	if reg.Len() != 0 {
		t.Errorf("registry len after close = %d", reg.Len())
	}
	if _, ok := reg.Close(ids[0], "again"); ok {
		t.Error("double close reported a live session")
	}

	var buf bytes.Buffer
	if _, err := reg.WriteMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	want := map[string]string{
		"duplex_sessions_active":    "0",
		"duplex_sessions_total":     fmt.Sprint(sessions),
		"duplex_in_frames_total":    fmt.Sprint(sessions * perSession.in),
		"duplex_out_frames_total":   fmt.Sprint(sessions * perSession.in),
		"duplex_late_inputs_total":  fmt.Sprint(sessions * perSession.lateIn),
		"duplex_late_outputs_total": fmt.Sprint(sessions * perSession.lateOut),
		"duplex_dropped_total":      fmt.Sprint(sessions * perSession.drops),
		"duplex_text_deltas_total":  fmt.Sprint(sessions * perSession.text),
		"duplex_errors_total":       "",
	}
	for name, value := range want {
		if value == "" {
			continue
		}
		if !strings.Contains(text, name+" "+value+"\n") {
			t.Errorf("metrics missing %s %s:\n%s", name, value, text)
		}
	}
	if !strings.Contains(text, `duplex_dropped_total{reason="queue_full"} 128`) {
		t.Errorf("dropped reason series missing:\n%s", text)
	}
	if !strings.Contains(text, `duplex_errors_total{code="internal"} 64`) {
		t.Errorf("error code series missing:\n%s", text)
	}

	var again bytes.Buffer
	if _, err := reg.WriteMetrics(&again); err != nil {
		t.Fatal(err)
	}
	if again.String() != text {
		t.Errorf("metrics exposition is not deterministic:\n%s\n---\n%s", text, again.String())
	}
}

func TestSummariesAreSortedAndSnapshotIndependent(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	reg := NewRegistry(Config{Now: func() time.Time { return fixed }})
	reg.Start(duplex.SessionInfo{SessionID: "dup_b", Tenant: "loopback"})
	reg.Start(duplex.SessionInfo{SessionID: "dup_a", Tenant: "loopback"})

	got := reg.Summaries()
	if len(got) != 2 || got[0].SessionID != "dup_a" || got[1].SessionID != "dup_b" {
		t.Fatalf("summaries = %+v", got)
	}
	got[0].DropsByReason["injected"] = 1
	if s := reg.Summaries()[0]; len(s.DropsByReason) != 0 {
		t.Errorf("summary shares its map with the registry: %+v", s.DropsByReason)
	}
}

func TestSafetyStopsAreCountedByCodeAndRetired(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	reg := NewRegistry(Config{Now: func() time.Time { return fixed }})
	o := reg.Start(duplex.SessionInfo{SessionID: "dup_safety", Tenant: "loopback"})
	o.SafetyStop("loop")
	o.SafetyStop("loop")
	o.SafetyStop("term")

	var live bytes.Buffer
	if _, err := reg.WriteMetrics(&live); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`duplex_safety_stops_total{code="loop"} 2`,
		`duplex_safety_stops_total{code="term"} 1`,
	} {
		if !strings.Contains(live.String(), want) {
			t.Errorf("active metrics missing %q:\n%s", want, live.String())
		}
	}

	if _, ok := reg.Close("dup_safety", "safety_stop"); !ok {
		t.Fatal("close did not find the session")
	}
	var retired bytes.Buffer
	if _, err := reg.WriteMetrics(&retired); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retired.String(), `duplex_safety_stops_total{code="loop"} 2`) {
		t.Errorf("retired metrics lost the counter:\n%s", retired.String())
	}
}
