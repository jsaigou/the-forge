// SPDX-License-Identifier: Apache-2.0

package duplexsafety

import "testing"

func TestTermPolicyMatchesWholePhraseCaseInsensitively(t *testing.T) {
	p := NewTermPolicy([]string{"Bad Word"})
	if v := p.Observe("this is a bad"); v.Stop {
		t.Fatalf("stopped on a partial term: %+v", v)
	}
	v := p.Observe(" word in the stream")
	if !v.Stop || v.Code != CodeTerm {
		t.Fatalf("verdict = %+v, want a term stop", v)
	}
}

func TestTermPolicyDoesNotMatchInsideAWord(t *testing.T) {
	p := NewTermPolicy([]string{"ass"})
	for _, delta := range []string{"classic ", "grass ", "assassin "} {
		if v := p.Observe(delta); v.Stop {
			t.Fatalf("matched %q inside a longer word", delta)
		}
	}
	if v := p.Observe("you ass."); !v.Stop {
		t.Fatal("did not match the standalone term")
	}
}

func TestTermPolicyMatchesJapaneseSubstring(t *testing.T) {
	p := NewTermPolicy([]string{"暴力"})
	if v := p.Observe("これは暴力です"); !v.Stop {
		t.Fatal("did not match an unspaced Japanese term")
	}
}

func TestTermPolicyWithNoTermsDoesNothing(t *testing.T) {
	p := NewTermPolicy(nil)
	if v := p.Observe("anything at all"); v.Stop {
		t.Fatalf("empty policy stopped: %+v", v)
	}
}

type fakePolicy struct {
	calls   int
	verdict Verdict
}

func (f *fakePolicy) Observe(string) Verdict {
	f.calls++
	return f.verdict
}

func TestChainReturnsFirstStopAndStopsObserving(t *testing.T) {
	first := &fakePolicy{}
	second := &fakePolicy{verdict: Verdict{Stop: true, Code: "second", Reason: "second"}}
	third := &fakePolicy{verdict: Verdict{Stop: true, Code: "third", Reason: "third"}}

	v := NewChain(first, second, third).Observe("x")
	if !v.Stop || v.Code != "second" {
		t.Fatalf("verdict = %+v, want the second policy", v)
	}
	if first.calls != 1 || second.calls != 1 || third.calls != 0 {
		t.Fatalf("calls = %d/%d/%d", first.calls, second.calls, third.calls)
	}
}

func TestNilChainDoesNothing(t *testing.T) {
	var c *Chain
	if v := c.Observe("x"); v.Stop {
		t.Fatalf("nil chain stopped: %+v", v)
	}
	if v := NewChain(nil).Observe("x"); v.Stop {
		t.Fatalf("empty chain stopped: %+v", v)
	}
}
