// SPDX-License-Identifier: Apache-2.0

package duplexobs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

func testObs(t *testing.T, now *time.Time) *Obs {
	t.Helper()
	return New(duplex.SessionInfo{SessionID: "dup_01JTEST", Tenant: "loopback"}, Config{
		Now: func() time.Time { return *now },
	})
}

func TestCountersQueuesAndEvents(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := testObs(t, &now)

	o.Input(false)
	o.Input(false)
	o.Input(true)
	o.Output(100*time.Millisecond, false)
	o.Output(210*time.Millisecond, true)
	o.Queues(2, 1)
	o.Drop("queue_full")
	o.Observe(duplex.Event{Kind: duplex.EventText})
	o.Observe(duplex.Event{Kind: duplex.EventText})
	o.Observe(duplex.Event{Kind: duplex.EventError, Error: &duplex.Error{Code: duplex.ErrOutputOverflow}})
	o.Observe(duplex.Event{Kind: duplex.EventDone, Reason: "client_done"})
	o.SetGPU(1024, 2048)

	now = now.Add(10 * time.Second)
	s := o.Summary()

	if s.SessionID != "dup_01JTEST" || s.Tenant != "loopback" {
		t.Fatalf("identity = %q/%q", s.SessionID, s.Tenant)
	}
	if s.UptimeMS != 10_000 {
		t.Errorf("uptime = %d, want 10000", s.UptimeMS)
	}
	if s.InFrames != 3 || s.OutFrames != 2 {
		t.Errorf("frames = in %d out %d, want 3/2", s.InFrames, s.OutFrames)
	}
	if s.LateInputs != 1 || s.LateOutputs != 1 {
		t.Errorf("late = %d/%d, want 1/1", s.LateInputs, s.LateOutputs)
	}
	if s.QueueIn != 2 || s.QueueOut != 1 {
		t.Errorf("queues = %d/%d, want 2/1", s.QueueIn, s.QueueOut)
	}
	if s.Dropped != 1 || s.DropsByReason["queue_full"] != 1 {
		t.Errorf("drops = %d %+v, want one queue_full", s.Dropped, s.DropsByReason)
	}
	if s.TextDeltas != 2 {
		t.Errorf("text deltas = %d, want 2", s.TextDeltas)
	}
	if s.ErrorsByCode[duplex.ErrOutputOverflow] != 1 || s.LastError != duplex.ErrOutputOverflow {
		t.Errorf("errors = %+v last %q", s.ErrorsByCode, s.LastError)
	}
	if !s.Done || s.Closed {
		t.Errorf("done=%v closed=%v, want done and not closed", s.Done, s.Closed)
	}
	if s.CloseReason != "client_done" {
		t.Errorf("close reason = %q", s.CloseReason)
	}
	if s.GPUUsedBytes != 1024 || s.GPUPeakBytes != 2048 {
		t.Errorf("gpu = %d/%d", s.GPUUsedBytes, s.GPUPeakBytes)
	}
	if !s.Redaction.Enabled || !s.Redaction.ReferenceAudioWithheld {
		t.Errorf("redaction status = %+v", s.Redaction)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := testObs(t, &now)

	first := o.Close("user_hangup")
	if !first.Closed || first.CloseReason != "user_hangup" {
		t.Fatalf("first close = %+v", first)
	}
	second := o.Close("internal")
	if !second.Closed || second.CloseReason != "user_hangup" {
		t.Errorf("second close changed the reason: %+v", second)
	}
	o.Observe(duplex.Event{Kind: duplex.EventDone, Reason: "late_done"})
	if s := o.Summary(); !s.Done || s.CloseReason != "user_hangup" {
		t.Errorf("late done event changed the closed session: %+v", s)
	}
}

func TestPercentilesAreDeterministic(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := testObs(t, &now)
	for i := 1; i <= 10; i++ {
		o.Step(time.Duration(i) * time.Millisecond)
		o.Output(time.Duration(i*10)*time.Millisecond, false)
	}

	got := o.Summary()
	want := struct {
		stepP50, stepP95, stepMax float64
		e2eP50, e2eP95            float64
	}{6, 10, 10, 60, 100}
	if got.StepP50MS != want.stepP50 || got.StepP95MS != want.stepP95 || got.StepMaxMS != want.stepMax {
		t.Errorf("step = %v/%v/%v, want %v/%v/%v",
			got.StepP50MS, got.StepP95MS, got.StepMaxMS, want.stepP50, want.stepP95, want.stepMax)
	}
	if got.E2EP50MS != want.e2eP50 || got.E2EP95MS != want.e2eP95 {
		t.Errorf("e2e = %v/%v, want %v/%v", got.E2EP50MS, got.E2EP95MS, want.e2eP50, want.e2eP95)
	}
	if again := o.Summary(); !reflect.DeepEqual(got, again) {
		t.Errorf("two snapshots of the same samples differ:\n%+v\n%+v", got, again)
	}
}

func TestPercentileWindowWrapsDeterministically(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := New(duplex.SessionInfo{SessionID: "dup_s", Tenant: "loopback"}, Config{
		Now:            func() time.Time { return now },
		SampleCapacity: 4,
	})
	for i := 1; i <= 6; i++ {
		o.Step(time.Duration(i) * time.Millisecond)
	}
	s := o.Summary()
	// The window keeps 3,4,5,6: nearest-rank p50 is 5, p95 is 6, and max
	// remains the lifetime maximum.
	if s.StepP50MS != 5 || s.StepP95MS != 6 || s.StepMaxMS != 6 {
		t.Errorf("wrapped window = %v/%v/%v, want 5/6/6", s.StepP50MS, s.StepP95MS, s.StepMaxMS)
	}
}

func TestObserveDriverStatsFillsUntimedFields(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := testObs(t, &now)
	o.Observe(duplex.Event{Kind: duplex.EventStats, Stats: duplex.Stats{
		InFrames: 75, OutFrames: 74, LateInputs: 2, LateOutputs: 1,
		QueueIn: 3, QueueOut: 1, Dropped: 4,
		StepP50MS: 34.2, StepP95MS: 48, StepMaxMS: 51.1,
		E2EP50MS: 190, E2EP95MS: 240,
	}})
	s := o.Summary()
	if s.InFrames != 75 || s.OutFrames != 74 || s.LateInputs != 2 || s.LateOutputs != 1 {
		t.Errorf("counters not adopted: %+v", s)
	}
	if s.QueueIn != 3 || s.QueueOut != 1 || s.Dropped != 4 {
		t.Errorf("queues/drops not adopted: %+v", s)
	}
	if s.StepP50MS != 34.2 || s.StepP95MS != 48 || s.StepMaxMS != 51.1 {
		t.Errorf("step fallback = %v/%v/%v", s.StepP50MS, s.StepP95MS, s.StepMaxMS)
	}
	if s.E2EP50MS != 190 || s.E2EP95MS != 240 {
		t.Errorf("e2e fallback = %v/%v", s.E2EP50MS, s.E2EP95MS)
	}

	o.Step(10 * time.Millisecond)
	s = o.Summary()
	if s.StepP50MS != 10 || s.StepP95MS != 10 {
		t.Errorf("local samples must win over the fallback: %v/%v", s.StepP50MS, s.StepP95MS)
	}
	if s.StepMaxMS != 51.1 {
		t.Errorf("driver lifetime max lost: %v", s.StepMaxMS)
	}
}

func TestStatsPayloadMatchesProtocolShape(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := testObs(t, &now)
	o.Input(false)
	o.Output(190*time.Millisecond, false)

	first := o.StatsPayload()
	second := o.StatsPayload()
	if first.Seq != 0 || second.Seq != 1 {
		t.Errorf("seq = %d/%d, want 0/1", first.Seq, second.Seq)
	}
	if first.Type != "stats" {
		t.Errorf("type = %q", first.Type)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"type":"stats"`, `"uptime_ms"`, `"in_frames"`, `"out_frames"`,
		`"late_inputs"`, `"late_outputs"`, `"queue_in"`, `"queue_out"`,
		`"dropped"`, `"step_ms":{"p50":0,"p95":0,"max":0}`,
		`"end_to_end_ms":{"p50":190,"p95":190}`, `"gpu":{"used_bytes":0,"peak_bytes":0}`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("payload missing %s: %s", key, raw)
		}
	}
}

func TestContractStatsProjection(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := testObs(t, &now)
	o.Input(true)
	o.Step(12 * time.Millisecond)
	now = now.Add(1500 * time.Millisecond)
	s := o.Summary().ContractStats()
	if s.SessionID != "dup_01JTEST" || s.InFrames != 1 || s.LateInputs != 1 ||
		s.StepP50MS != 12 || s.Uptime != 1500*time.Millisecond {
		t.Errorf("contract projection = %+v", s)
	}
}
