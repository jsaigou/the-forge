package main

import "testing"

func TestEnvBoolFalseSpellingsAreCaseInsensitive(t *testing.T) {
	for v, want := range map[string]bool{"false": false, "FALSE": false, "False": false, "0": false, "no": false, "OFF": false, " off ": false,
		"true": true, "1": true, "yes": true, "anything": true} {
		t.Setenv("FORGE_TTS_ENVBOOL_TEST", v)
		if got := envBool("FORGE_TTS_ENVBOOL_TEST", !want); got != want {
			t.Errorf("envBool(%q) = %v, want %v", v, got, want)
		}
	}
	t.Setenv("FORGE_TTS_ENVBOOL_TEST", "")
	if !envBool("FORGE_TTS_ENVBOOL_TEST", true) || envBool("FORGE_TTS_ENVBOOL_TEST", false) {
		t.Error("an empty value must return the default")
	}
}
