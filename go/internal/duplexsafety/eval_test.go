// SPDX-License-Identifier: Apache-2.0

package duplexsafety

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

type evalCase struct {
	Name       string   `json:"name"`
	Label      string   `json:"label"`
	Why        string   `json:"why"`
	ClearAfter int      `json:"clear_after"`
	Deltas     []string `json:"deltas"`
}

type evalFile struct {
	Cases []evalCase `json:"cases"`
}

// maxDeltasAfterClear is the bound the detector must meet once a loop is
// unambiguous. Deltas are tiny, so at most a few more are allowed for word
// completion and the next stream event.
const maxDeltasAfterClear = 4

func loadEval(t *testing.T) evalFile {
	t.Helper()
	data, err := os.ReadFile("testdata/transcripts.json")
	if err != nil {
		t.Fatalf("read eval set: %v", err)
	}
	var ef evalFile
	if err := json.Unmarshal(data, &ef); err != nil {
		t.Fatalf("parse eval set: %v", err)
	}
	return ef
}

func TestEvalSet(t *testing.T) {
	ef := loadEval(t)
	loops, normals := 0, 0

	for _, c := range ef.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if c.Why == "" {
				t.Fatal("case has no note explaining why")
			}
			det := NewLoopDetector(Default())
			stopAt := 0
			for i, delta := range c.Deltas {
				if det.Observe(delta).Stop {
					stopAt = i + 1
					break
				}
			}

			switch c.Label {
			case "loop":
				loops++
				if stopAt == 0 {
					t.Fatalf("loop not stopped after %d deltas: %s", len(c.Deltas), c.Why)
				}
				if c.ClearAfter <= 0 || c.ClearAfter > len(c.Deltas) {
					t.Fatalf("bad clear_after %d for %d deltas", c.ClearAfter, len(c.Deltas))
				}
				if stopAt > c.ClearAfter+maxDeltasAfterClear {
					t.Fatalf("stop at delta %d, want <= %d (clear at %d)",
						stopAt, c.ClearAfter+maxDeltasAfterClear, c.ClearAfter)
				}
			case "normal":
				normals++
				if stopAt != 0 {
					t.Fatalf("normal transcript stopped at delta %d: %s", stopAt, c.Why)
				}
			default:
				t.Fatalf("unknown label %q", c.Label)
			}
		})
	}

	if loops < 12 {
		t.Errorf("eval set has %d loops, want at least 12", loops)
	}
	if normals < 20 {
		t.Errorf("eval set has %d normal transcripts, want at least 20", normals)
	}
}

func TestJapaneseNoSpacesRuneByRune(t *testing.T) {
	normal := "今日はいい天気ですねそうですねありがとうございます"
	det := NewLoopDetector(Default())
	for _, r := range normal {
		if v := det.Observe(string(r)); v.Stop {
			t.Fatalf("normal Japanese stopped at %q", r)
		}
	}

	phrase := "きょうはいいてんきですね"
	det = NewLoopDetector(Default())
	stopAt := 0
	n := 0
	for rep := 0; rep < 5 && stopAt == 0; rep++ {
		for _, r := range phrase {
			n++
			if det.Observe(string(r)).Stop {
				stopAt = n
				break
			}
		}
	}
	if stopAt == 0 {
		t.Fatal("Japanese no-space loop was not stopped")
	}
	bound := 4*len([]rune(phrase)) + maxDeltasAfterClear
	if stopAt > bound {
		t.Fatalf("stop after %d rune deltas, want <= %d", stopAt, bound)
	}
}

func TestLongNormalStreamThenLoop(t *testing.T) {
	det := NewLoopDetector(Default())
	for i := 0; i < 4000; i++ {
		if v := det.Observe("word" + strconv.Itoa(i) + " "); v.Stop {
			t.Fatalf("long normal stream stopped at %d", i)
		}
	}
	for i := 0; i < 4; i++ {
		v := det.Observe("this is a long repeated sentence ")
		if i == 3 && !v.Stop {
			t.Fatal("loop after a long stream was not stopped")
		}
		if v.Stop && i < 3 {
			t.Fatalf("loop stopped too early at repeat %d", i)
		}
	}
}
