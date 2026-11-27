package main

import (
	"reflect"
	"testing"
)

func TestIgnoredRetiredVarsNamesOnlyTheOnesThatAreSet(t *testing.T) {
	for _, k := range retiredVars {
		t.Setenv(k, "")
	}
	if got := ignoredRetiredVars(); len(got) != 0 {
		t.Fatalf("nothing set, got %v", got)
	}
	t.Setenv("FORGE_TTS_INFERENCE", "server")
	t.Setenv("FORGE_TTS_BIN_DIR", "/opt/x")
	t.Setenv("FORGE_TTS_DAEMON_URL", "http://127.0.0.1:5000") // a live variable is never reported
	want := []string{"FORGE_TTS_BIN_DIR", "FORGE_TTS_INFERENCE"}
	if got := ignoredRetiredVars(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
