package tts

import (
	"testing"
)

func TestCLIBackendArgs(t *testing.T) {
	c := NewCLIBackend("/bin", "/models", "1.7b", "Vulkan0", "tok.gguf")

	cv, err := c.args(SynthSpec{Mode: ModeCustomVoice, Speaker: "aiden", Language: "English"})
	if err != nil {
		t.Fatalf("customvoice args: %v", err)
	}
	if !hasFlag(cv, "--speaker", "aiden") {
		t.Fatalf("customvoice missing speaker: %v", cv)
	}
	if !hasFlag(cv, "--model", "/models/qwen-talker-1.7b-customvoice-Q8_0.gguf") {
		t.Fatalf("customvoice wrong model: %v", cv)
	}

	vd, err := c.args(SynthSpec{Mode: ModeVoiceDesign, Instruct: "warm", Language: "English"})
	if err != nil {
		t.Fatalf("voicedesign args: %v", err)
	}
	if !hasFlag(vd, "--instruct", "warm") {
		t.Fatalf("voicedesign missing instruct: %v", vd)
	}

	base, err := c.args(SynthSpec{Mode: ModeBase, RefAudio: []byte("wav"), RefText: "hi", Language: "English"})
	if err != nil {
		t.Fatalf("base args: %v", err)
	}
	refTextIdx := -1
	for i := 0; i+1 < len(base); i++ {
		if base[i] == "--ref-text" {
			refTextIdx = i + 1
		}
	}
	if refTextIdx == -1 || base[refTextIdx] == "" {
		t.Fatalf("base missing ref-text file: %v", base)
	}
	foundRef := false
	for i, a := range base {
		if a == "--ref-wav" && i+1 < len(base) {
			foundRef = true
		}
	}
	if !foundRef {
		t.Fatalf("base missing ref-wav: %v", base)
	}

	if _, err := c.args(SynthSpec{Mode: ModeBase}); err == nil {
		t.Fatalf("expected error for base without ref audio")
	}
	if _, err := c.args(SynthSpec{Mode: "bogus"}); err == nil {
		t.Fatalf("expected error for unknown mode")
	}
}

func hasFlag(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
