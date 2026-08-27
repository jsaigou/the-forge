package tts

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// cliBackend shells the qwen-tts CLI. The model is reloaded on every call
// (qwen-tts is a one-shot binary), so this backend is correct but not the
// lowest-latency option. serverBackend fronts the resident tts-server instead.
type cliBackend struct {
	binDir    string
	modelsDir string
	size      string
	backend   string
	tokenizer string
}

func NewCLIBackend(binDir, modelsDir, size, backend, tokenizer string) *cliBackend {
	if size == "" {
		size = "1.7b"
	}
	if backend == "" {
		backend = "Vulkan0"
	}
	if tokenizer == "" {
		tokenizer = "qwen-tokenizer-12hz-Q8_0.gguf"
	}
	return &cliBackend{
		binDir:    binDir,
		modelsDir: modelsDir,
		size:      size,
		backend:   backend,
		tokenizer: tokenizer,
	}
}

func (c *cliBackend) talker(mode VoiceMode) string {
	return filepath.Join(c.modelsDir, fmt.Sprintf("qwen-talker-%s-%s-Q8_0.gguf", c.size, mode))
}

func (c *cliBackend) codec() string {
	return filepath.Join(c.modelsDir, c.tokenizer)
}

// Request-derived values below become argv of the qwen-tts subprocess.
// argv exec is not shell-injectable, but a value shaped like a flag could
// still be parsed as one (argument injection), so each gets a strict shape
// check at this boundary.
var (
	cliLangRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z .,'-]*$`)
	cliSpeakerRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func (c *cliBackend) args(spec SynthSpec) ([]string, error) {
	lang := spec.Language
	if lang == "" {
		lang = "English"
	}
	if !cliLangRe.MatchString(lang) {
		return nil, fmt.Errorf("unsupported language %q", spec.Language)
	}
	args := []string{
		"--model", c.talker(spec.Mode),
		"--codec", c.codec(),
		"--lang", lang,
	}
	switch spec.Mode {
	case ModeCustomVoice:
		if spec.Speaker == "" {
			return nil, fmt.Errorf("customvoice requires a speaker")
		}
		if !cliSpeakerRe.MatchString(spec.Speaker) {
			return nil, fmt.Errorf("invalid speaker %q", spec.Speaker)
		}
		args = append(args, "--speaker", spec.Speaker)
	case ModeVoiceDesign:
		if strings.ContainsAny(spec.Instruct, "\x00\r\n") || strings.HasPrefix(spec.Instruct, "-") {
			return nil, fmt.Errorf("invalid instruct text")
		}
		args = append(args, "--instruct", spec.Instruct)
	case ModeBase:
		if len(spec.RefAudio) == 0 {
			return nil, fmt.Errorf("base clone requires reference audio")
		}
		refWav, err := os.CreateTemp("", "tts-ref-*.wav")
		if err != nil {
			return nil, err
		}
		if _, err := refWav.Write(spec.RefAudio); err != nil {
			refWav.Close()
			return nil, err
		}
		refWav.Close()
		refText, err := os.CreateTemp("", "tts-ref-*.txt")
		if err != nil {
			return nil, err
		}
		if _, err := refText.WriteString(spec.RefText); err != nil {
			refText.Close()
			return nil, err
		}
		refText.Close()
		args = append(args, "--ref-wav", refWav.Name(), "--ref-text", refText.Name())
	default:
		return nil, fmt.Errorf("unsupported mode %s", spec.Mode)
	}
	return args, nil
}

func (c *cliBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	args, err := c.args(spec)
	if err != nil {
		return nil, err
	}
	out, err := os.CreateTemp("", "tts-*.wav")
	if err != nil {
		return nil, err
	}
	outPath := out.Name()
	out.Close()
	defer os.Remove(outPath)
	args = append(args, "-o", outPath)

	cmd := exec.CommandContext(ctx, filepath.Join(c.binDir, "qwen-tts"), args...)
	cmd.Env = append(os.Environ(), "GGML_BACKEND="+c.backend)
	cmd.Stdin = strings.NewReader(text)
	outb, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("cli speech error: %v: %s", err, tail(outb, 400))
		return nil, fmt.Errorf("qwen-tts: %w: %s", err, string(outb))
	}
	log.Printf("cli speech ok: mode=%s inputLen=%d", spec.Mode, len(text))
	wav, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	format := "wav"
	if len(wav) > 44 && strings.EqualFold(spec.Format, "pcm") {
		return &SpeechResult{Audio: wav[44:], Format: "pcm"}, nil
	}
	return &SpeechResult{Audio: wav, Format: format}, nil
}

func (c *cliBackend) Health(ctx context.Context) (map[string]any, error) {
	if _, err := os.Stat(filepath.Join(c.binDir, "qwen-tts")); err != nil {
		return nil, fmt.Errorf("qwen-tts not found: %w", err)
	}
	return map[string]any{"status": "ok", "backend": c.backend, "engine": "qwentts-cli"}, nil
}

func (c *cliBackend) Close() error { return nil }

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}
