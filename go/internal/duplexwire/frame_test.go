package duplexwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/jsaigou/the-forge/internal/duplex"
)

func TestAudioFrameRoundTrip(t *testing.T) {
	pcm := make([]byte, FramePayloadBytes)
	values := []int16{0, 1, -1, 32767, -32768, 1234, -4321}
	for i := 0; i < duplex.FrameSamples; i++ {
		v := values[i%len(values)]
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}

	msg, err := EncodeAudioFrame(42, pcm)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(msg) != FrameBytes {
		t.Fatalf("encoded length = %d, want %d", len(msg), FrameBytes)
	}

	seq, got, err := DecodeAudioFrame(msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if seq != 42 {
		t.Fatalf("seq = %d, want 42", seq)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatal("payload changed in round trip")
	}

	frame, err := FrameFromPCM(got)
	if err != nil {
		t.Fatalf("FrameFromPCM: %v", err)
	}
	if !frame.Valid() {
		t.Fatalf("frame has %d samples", len(frame))
	}
	back, err := PCMFromFrame(frame)
	if err != nil {
		t.Fatalf("PCMFromFrame: %v", err)
	}
	if !bytes.Equal(back, pcm) {
		t.Fatal("PCM -> float32 -> PCM is not bit-exact")
	}
}

func TestDecodeAudioFrameRejects(t *testing.T) {
	valid := make([]byte, FrameBytes)
	valid[0] = AudioKind

	cases := []struct {
		name string
		b    []byte
		want error
	}{
		{"short header", valid[:4], ErrFrameTooShort},
		{"short payload", valid[:FrameBytes-1], ErrFrameTooShort},
		{"long payload", append(valid, 0), ErrFrameTooLong},
		{"bad kind", append([]byte{AudioKind + 1}, valid[1:]...), ErrBadKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := DecodeAudioFrame(tc.b); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPCMWrongLength(t *testing.T) {
	if _, err := FrameFromPCM(make([]byte, 10)); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("FrameFromPCM err = %v", err)
	}
	if _, err := PCMFromFrame(duplex.Frame{1, 2, 3}); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("PCMFromFrame err = %v", err)
	}
	if _, err := EncodeAudioFrame(0, make([]byte, 10)); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("EncodeAudioFrame err = %v", err)
	}
}

func TestSequencer(t *testing.T) {
	var s Sequencer
	for i := uint32(0); i < 5; i++ {
		if err := s.Check(i); err != nil {
			t.Fatalf("Check(%d): %v", i, err)
		}
	}
	if err := s.Check(4); !errors.Is(err, ErrSequenceDuplicate) {
		t.Fatalf("duplicate err = %v", err)
	}
	if err := s.Check(9); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("gap err = %v", err)
	}

	var fresh Sequencer
	if err := fresh.Check(1); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("first non-zero err = %v", err)
	}
}

func TestOutputSequencer(t *testing.T) {
	var s Sequencer
	for i := uint32(0); i < 3; i++ {
		if got := s.Next(); got != i {
			t.Fatalf("Next() = %d, want %d", got, i)
		}
	}
}
