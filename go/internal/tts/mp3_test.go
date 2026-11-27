// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// speechWAV is a 16-bit mono WAV of a tone that starts at full level on the first sample and ends while still loud: the worst case for an encoder.
func speechWAV(seconds float64, rate int) []byte {
	n := int(seconds * float64(rate))
	pcm := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := 0.4 * math.Sin(2*math.Pi*300*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(math.Round(v*32767))))
	}
	return append(wavHeader(len(pcm), rate), pcm...)
}

func postSpeech(t *testing.T, ts *httptest.Server, format string) (*http.Response, []byte) {
	t.Helper()
	body := `{"input":"hello","voice":"v","response_format":"` + format + `"}`
	resp, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed here")
	}
}

// A request for mp3 gets an mp3, not WAV bytes under an mp3 label, and the encoder's padding is described so a player trims exactly the padding.
func TestMP3_ARequestForMP3GetsARealMP3(t *testing.T) {
	needFFmpeg(t)
	srv, fe := newTestServer("")
	wav := speechWAV(2, 44100)
	fe.speech = &SpeechResult{Audio: wav, Format: "wav"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, mp3 := postSpeech(t, ts, "mp3")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("HTTP %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if bytes.HasPrefix(mp3, []byte("RIFF")) {
		t.Fatal("the answer to an mp3 request is a RIFF file")
	}
	if !(bytes.HasPrefix(mp3, []byte("ID3")) || (len(mp3) > 2 && mp3[0] == 0xFF && mp3[1]&0xE0 == 0xE0)) {
		t.Fatalf("the answer does not start like an mp3: % x", mp3[:8])
	}
	if !bytes.Contains(mp3[:4096], []byte("Info")) && !bytes.Contains(mp3[:4096], []byte("Xing")) {
		t.Error("the mp3 has no Xing/Info header: a player cannot trim the encoder delay and padding exactly")
	}

	// Decoded, it is as long as the WAV (the header's delay and padding were honoured) and the first sample is not lost.
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/a.mp3", mp3, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", dir+"/a.mp3", "-f", "f32le", "-ac", "1", "-ar", "44100", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := float64(len(out)/4) / 44100
	if math.Abs(got-2) > 0.005 {
		t.Errorf("the decoded mp3 is %.4f s, the WAV 2.0000 s: the padding was not described exactly", got)
	}
	peak := 0.0
	for i := 0; i < 44100/50; i++ { // the first 20 ms
		peak = math.Max(peak, math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(out[4*i:])))))
	}
	if peak < 0.3 {
		t.Errorf("the first 20 ms of the decoded mp3 peak at %.3f against 0.4 in the WAV: the start was chopped", peak)
	}
}

// Without ffmpeg the answer is an error that says so, never WAV bytes that look like what was asked for.
func TestMP3_WithoutFFmpegIsAnErrorNotAWav(t *testing.T) {
	srv, fe := newTestServer("")
	fe.speech = &SpeechResult{Audio: speechWAV(0.5, 24000), Format: "wav"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	t.Setenv("PATH", "")
	resp, body := postSpeech(t, ts, "mp3")
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "ffmpeg") {
		t.Errorf("HTTP %d %q, want a 500 that names ffmpeg", resp.StatusCode, body)
	}
	if strings.HasPrefix(string(body), "RIFF") || resp.Header.Get("Content-Type") == "audio/wav" {
		t.Error("an mp3 request that could not be served answered with WAV")
	}
}

// Other formats are untouched.
func TestMP3_WAVRequestsStayWAV(t *testing.T) {
	srv, fe := newTestServer("")
	wav := speechWAV(0.5, 24000)
	fe.speech = &SpeechResult{Audio: wav, Format: "wav"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, f := range []string{"wav", ""} {
		resp, b := postSpeech(t, ts, f)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || !bytes.Equal(b, wav) {
			t.Errorf("format %q: HTTP %d %q, %d bytes", f, resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
		}
	}
}
