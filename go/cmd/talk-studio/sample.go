package main

// A voice sample made for one conversation.
//
// Haru hears its reference clip as its own earlier speech, so the words in a recording of a real person (a name, a greeting, a topic) come back in
// the conversation (docs/duplex/haru-ref-eval-2026-11-24.md). Instead of feeding it a recording as it is, talk-studio can ask voice-studio, which already
// owns the cloned voices and the TTS engines, to speak a short text written for the conversation (the card's name, the other person) in the chosen voice,
// and uses that audio as the reference. The audio is converted to the canonical 24 kHz mono PCM16 WAV the engine reads and cached by (voice, text).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/duplexwire"
)

const (
	sampleMaxText       = 600
	sampleMinSeconds    = 3.0 // Haru wants 3 s to 3 min of reference
	sampleMaxSeconds    = 60.0
	sampleMaxAudioBytes = 16 << 20
	sampleJobTimeout    = 120 * time.Second
)

var vsVoicePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// sampleID names a sample by its inputs, so the same voice and text are made once.
func sampleID(voice, text string) string {
	h := sha256.Sum256([]byte(voice + "\x00" + text))
	return hex.EncodeToString(h[:6])
}

// vsClient is the client for voice-studio calls: the server-wide client has a short overall timeout, a sample takes longer.
func (s *Server) vsClient() *http.Client { return &http.Client{Transport: s.o.Client.Transport} }

func (s *Server) vsDo(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.o.VoiceStudioURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.vsClient().Do(req)
}

// handleVSVoices lists the voices voice-studio can speak with, so the page offers them without calling another origin.
func (s *Server) handleVSVoices(w http.ResponseWriter, r *http.Request) {
	if s.o.VoiceStudioURL == "" {
		writeErr(w, 404, "voice_studio_not_configured", "no voice-studio is configured for this server (TALK_STUDIO_VOICE_STUDIO_URL)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	resp, err := s.vsDo(ctx, "GET", "/api/voices", nil)
	if err != nil {
		writeErr(w, 502, "voice_studio_unreachable", "voice-studio did not answer: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, "voice_studio_error", fmt.Sprintf("voice-studio answered HTTP %d", resp.StatusCode))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		writeErr(w, 502, "voice_studio_error", "could not read voice-studio's voice list: "+err.Error())
		return
	}
	type vsVoice struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Language string `json:"language"`
		Builtin  bool   `json:"builtin"`
	}
	var wrapped struct {
		Voices []vsVoice `json:"voices"`
	}
	var list []vsVoice
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Voices != nil {
		list = wrapped.Voices
	} else if err := json.Unmarshal(raw, &list); err != nil {
		writeErr(w, 502, "voice_studio_error", "voice-studio's voice list is not in the expected form")
		return
	}
	out := make([]vsVoice, 0, len(list))
	for _, v := range list {
		if vsVoicePattern.MatchString(v.ID) {
			out = append(out, v)
		}
	}
	writeJSON(w, 200, out)
}

// handleMakeSample speaks req.Text in req.Voice through voice-studio and stores the result as a reference clip.
func (s *Server) handleMakeSample(w http.ResponseWriter, r *http.Request) {
	if s.o.VoiceStudioURL == "" {
		writeErr(w, 404, "voice_studio_not_configured", "no voice-studio is configured for this server (TALK_STUDIO_VOICE_STUDIO_URL)")
		return
	}
	var req struct {
		Voice string `json:"voice"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, 400, "bad_request", `the body must be JSON like {"voice":"...","text":"..."}`)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if !vsVoicePattern.MatchString(req.Voice) {
		writeErr(w, 422, "invalid_sample", "choose a voice")
		return
	}
	if req.Text == "" || len(req.Text) > sampleMaxText {
		writeErr(w, 422, "invalid_sample", fmt.Sprintf("the sample text must be 1 to %d characters", sampleMaxText))
		return
	}
	id := sampleID(req.Voice, req.Text)
	if raw, err := os.ReadFile(s.o.Store.path("samples", id+".wav")); err == nil {
		writeJSON(w, 200, map[string]any{"id": id, "seconds": wavSeconds(raw), "cached": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sampleJobTimeout)
	defer cancel()

	resp, err := s.vsDo(ctx, "POST", "/api/generate", map[string]any{"input": req.Text, "voice": req.Voice, "response_format": "wav"})
	if err != nil {
		writeErr(w, 502, "voice_studio_unreachable", "voice-studio did not answer: "+err.Error())
		return
	}
	var gen struct {
		JobID string `json:"job_id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&gen)
	resp.Body.Close()
	if resp.StatusCode >= 300 || derr != nil || gen.JobID == "" {
		msg := gen.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("voice-studio refused the request (HTTP %d)", resp.StatusCode)
		}
		writeErr(w, 502, "voice_studio_error", msg)
		return
	}
	jobPath := "/api/jobs/" + url.PathEscape(gen.JobID)
	// The job is removed from voice-studio when this request ends, whatever happened, so samples do not pile up in its history. On the success path a
	// failed removal is reported in the response; on a failure path it is logged (the request already carries its own error).
	removed := false
	removeJob := func() string {
		removed = true
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		dr, err := s.vsDo(dctx, "DELETE", jobPath, nil)
		if err != nil {
			return err.Error()
		}
		dr.Body.Close()
		if dr.StatusCode >= 300 && dr.StatusCode != 404 {
			return fmt.Sprintf("HTTP %d", dr.StatusCode)
		}
		return ""
	}
	defer func() {
		if !removed {
			if msg := removeJob(); msg != "" {
				log.Printf("talk-studio: could not remove voice-studio job %s after a failed sample: %s", gen.JobID, msg)
			}
		}
	}()

	for {
		jr, err := s.vsDo(ctx, "GET", jobPath, nil)
		if err != nil {
			writeErr(w, 502, "voice_studio_unreachable", "voice-studio stopped answering while the sample was being made: "+err.Error())
			return
		}
		var j struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(jr.Body, 1<<20)).Decode(&j)
		jr.Body.Close()
		if j.State == "failed" {
			writeErr(w, 502, "voice_studio_error", "voice-studio could not make the sample: "+j.Error)
			return
		}
		if j.State == "complete" {
			break
		}
		select {
		case <-ctx.Done():
			writeErr(w, 504, "voice_studio_timeout", "voice-studio did not finish the sample in time")
			return
		case <-time.After(700 * time.Millisecond):
		}
	}
	ar, err := s.vsDo(ctx, "GET", jobPath+"/audio", nil)
	if err != nil {
		writeErr(w, 502, "voice_studio_unreachable", "could not fetch the sample: "+err.Error())
		return
	}
	defer ar.Body.Close()
	if ar.StatusCode != 200 {
		writeErr(w, 502, "voice_studio_error", fmt.Sprintf("could not fetch the sample (HTTP %d)", ar.StatusCode))
		return
	}
	audio, err := io.ReadAll(io.LimitReader(ar.Body, sampleMaxAudioBytes+1))
	if err != nil || len(audio) > sampleMaxAudioBytes {
		writeErr(w, 502, "voice_studio_error", "the sample audio could not be read or is too large")
		return
	}
	wav, err := canonicalWAV(audio)
	if err != nil {
		writeErr(w, 502, "voice_studio_error", "the sample is not audio the engine can use: "+err.Error())
		return
	}
	secs := wavSeconds(wav)
	if secs < sampleMinSeconds {
		writeErr(w, 422, "sample_too_short", fmt.Sprintf("the sample came out %.1f s long and Haru needs at least %.0f s: use a longer text", secs, sampleMinSeconds))
		return
	}
	if secs > sampleMaxSeconds || len(wav) > duplexwire.MaxReferenceAudioBytes {
		writeErr(w, 422, "sample_too_long", "the sample is too long for a reference: use a shorter text")
		return
	}
	if err := s.o.Store.putSample(id, wav); err != nil {
		writeErr(w, 500, "internal", "could not store the sample: "+err.Error())
		return
	}
	out := map[string]any{"id": id, "seconds": secs, "cached": false}
	if msg := removeJob(); msg != "" {
		out["cleanup_warning"] = "the sample is ready, but its job could not be removed from voice-studio: " + msg
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSampleAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		writeErr(w, 404, "not_found", "no such sample")
		return
	}
	b, err := os.ReadFile(s.o.Store.path("samples", id+".wav"))
	if err != nil {
		writeErr(w, 404, "not_found", "no such sample")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "sample.wav", time.Time{}, bytes.NewReader(b))
}

// putSample stores a sample atomically (a half-written file must never be served as a reference).
func (s *Store) putSample(id string, wav []byte) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	if err := os.MkdirAll(s.path("samples"), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.path("samples"), id+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(wav); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path("samples", id+".wav"))
}

func wavSeconds(canonical []byte) float64 {
	if len(canonical) <= wavHeaderBytes {
		return 0
	}
	return float64(len(canonical)-wavHeaderBytes) / (sampleRate * 2)
}

// canonicalWAV converts a WAV file (PCM 16-bit, 24-bit, 32-bit or float32, any channel count and rate, extra chunks allowed) to the canonical 44-byte-header
// mono 16-bit 24 kHz PCM WAV the engine reads: channels are averaged and the rate is converted by linear interpolation.
func canonicalWAV(b []byte) ([]byte, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var format, channels, bits uint16
	var rate uint32
	var data []byte
	haveFmt := false
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := p + 8
		end := body + size
		if end > len(b) || size < 0 {
			end = len(b) // a streamed WAV may declare more than it holds
		}
		switch id {
		case "fmt ":
			if end-body < 16 {
				return nil, errors.New("the fmt chunk is too short")
			}
			format = binary.LittleEndian.Uint16(b[body:])
			channels = binary.LittleEndian.Uint16(b[body+2:])
			rate = binary.LittleEndian.Uint32(b[body+4:])
			bits = binary.LittleEndian.Uint16(b[body+14:])
			if format == 0xFFFE && end-body >= 26 { // WAVE_FORMAT_EXTENSIBLE: the real format is the first two bytes of the sub-format GUID
				format = binary.LittleEndian.Uint16(b[body+24:])
			}
			haveFmt = true
		case "data":
			data = b[body:end]
		}
		if data != nil && haveFmt {
			break
		}
		p = end + (size & 1)
	}
	if !haveFmt || data == nil {
		return nil, errors.New("the WAV has no fmt or data chunk")
	}
	if channels == 0 || rate == 0 {
		return nil, errors.New("the WAV declares no channels or no sample rate")
	}
	var bytesPer int
	switch {
	case format == 1 && (bits == 16 || bits == 24 || bits == 32):
		bytesPer = int(bits) / 8
	case format == 3 && bits == 32:
		bytesPer = 4
	default:
		return nil, fmt.Errorf("unsupported WAV format (tag %d, %d bits)", format, bits)
	}
	frame := bytesPer * int(channels)
	n := len(data) / frame
	if n == 0 {
		return nil, errors.New("the WAV holds no audio")
	}
	mono := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		for c := 0; c < int(channels); c++ {
			o := i*frame + c*bytesPer
			switch {
			case format == 3:
				sum += float64(math.Float32frombits(binary.LittleEndian.Uint32(data[o:])))
			case bits == 16:
				sum += float64(int16(binary.LittleEndian.Uint16(data[o:]))) / 32768
			case bits == 24:
				v := int32(data[o]) | int32(data[o+1])<<8 | int32(int8(data[o+2]))<<16
				sum += float64(v) / 8388608
			default:
				sum += float64(int32(binary.LittleEndian.Uint32(data[o:]))) / 2147483648
			}
		}
		mono[i] = sum / float64(channels)
	}
	outN := n
	if rate != sampleRate {
		outN = int(math.Round(float64(n) * sampleRate / float64(rate)))
	}
	if outN == 0 {
		return nil, errors.New("the WAV is too short")
	}
	pcm := make([]byte, outN*2)
	for i := 0; i < outN; i++ {
		var v float64
		if rate == sampleRate {
			v = mono[i]
		} else {
			pos := float64(i) * float64(rate) / sampleRate
			k := int(pos)
			frac := pos - float64(k)
			a := mono[min(k, n-1)]
			bb := mono[min(k+1, n-1)]
			v = a + (bb-a)*frac
		}
		v = math.Max(-1, math.Min(1, v))
		var s16 int16
		if v < 0 {
			s16 = int16(math.Round(v * 32768))
		} else {
			s16 = int16(math.Round(v * 32767))
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s16))
	}
	return append(WAVHeader(len(pcm)), pcm...), nil
}
