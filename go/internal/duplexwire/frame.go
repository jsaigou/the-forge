// SPDX-License-Identifier: Apache-2.0

// Package duplexwire is the v0 wire codec for the duplex audio endpoint
// (docs/duplex/protocol.md §1–§3). It owns the JSON control messages and the
// binary PCM16 frame layout; the transport in cmd/forge-duplex owns sockets,
// auth and the session state machine. The tenant never sees PCM16 — the
// transport converts to the platform's float32 frame before the driver.
package duplexwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/jsaigou/the-forge/internal/duplex"
)

const (
	// Version is the protocol version carried in session.start/ready.
	Version = 1

	// AudioKind is the binary frame kind byte (0x01 = audio).
	AudioKind byte = 0x01

	// MaxBinaryMessage is the largest accepted WebSocket binary message.
	MaxBinaryMessage = 128 << 10
	// MaxControlMessage is the largest accepted WebSocket text message, session.start excepted.
	MaxControlMessage = 64 << 10
	// MaxStartMessage is the largest accepted session.start: it may carry the reference voice inline as base64 (MaxReferenceAudioBytes decoded is
	// about 2.8 MB encoded), which the 64 KiB control limit made impossible. Only the first message may be this large; the connection is already
	// authenticated when it arrives.
	MaxStartMessage = 4 << 20

	// MaxCardBytes caps the opaque tenant card passed through by the platform.
	MaxCardBytes = 8 << 10
	// MaxReferenceAudioBytes caps the decoded reference audio (2 MiB is about 44 s of 24 kHz PCM16). The engine states its own, smaller or larger,
	// limit (max_reference_bytes) and the remote tenant enforces it too.
	MaxReferenceAudioBytes = 2 << 20
	// MaxTranscriptBytes caps a reference transcript.
	MaxTranscriptBytes = 8 << 10
	// MaxClientFieldBytes caps client name/version strings.
	MaxClientFieldBytes = 256
	// MaxTenantNameBytes caps a tenant name.
	MaxTenantNameBytes = 64

	// FrameHeaderBytes is kind + u32 sequence.
	FrameHeaderBytes = 5
	// FramePayloadBytes is 3200 PCM16 samples.
	FramePayloadBytes = duplex.FrameSamples * 2
	// FrameBytes is the exact size of one binary audio message.
	FrameBytes = FrameHeaderBytes + FramePayloadBytes
)

var (
	// ErrFrameTooShort and ErrFrameTooLong describe a wrong binary length.
	ErrFrameTooShort = errors.New("duplexwire: binary frame shorter than 6405 bytes")
	ErrFrameTooLong  = errors.New("duplexwire: binary frame longer than 6405 bytes")
	// ErrBadKind is a binary frame whose kind byte is not 0x01.
	ErrBadKind = errors.New("duplexwire: binary frame kind is not audio (0x01)")
	// ErrBadPayload is a PCM conversion with the wrong byte/sample count.
	ErrBadPayload = errors.New("duplexwire: PCM payload must be exactly 6400 bytes")
	// ErrSequenceGap and ErrSequenceDuplicate are sequence check failures.
	ErrSequenceGap       = errors.New("duplexwire: sequence gap")
	ErrSequenceDuplicate = errors.New("duplexwire: duplicate sequence")
)

// EncodeAudioFrame builds one 6405-byte binary audio message. pcm must be
// exactly 6400 bytes of little-endian PCM16.
func EncodeAudioFrame(seq uint32, pcm []byte) ([]byte, error) {
	if len(pcm) != FramePayloadBytes {
		return nil, fmt.Errorf("%w: got %d", ErrBadPayload, len(pcm))
	}
	b := make([]byte, FrameBytes)
	b[0] = AudioKind
	binary.LittleEndian.PutUint32(b[1:5], seq)
	copy(b[5:], pcm)
	return b, nil
}

// DecodeAudioFrame splits a binary message into its sequence and PCM payload.
// The returned payload aliases b. It rejects the wrong kind or length.
func DecodeAudioFrame(b []byte) (seq uint32, pcm []byte, err error) {
	if len(b) < FrameHeaderBytes {
		return 0, nil, ErrFrameTooShort
	}
	if b[0] != AudioKind {
		return 0, nil, ErrBadKind
	}
	if len(b) < FrameBytes {
		return 0, nil, ErrFrameTooShort
	}
	if len(b) > FrameBytes {
		return 0, nil, ErrFrameTooLong
	}
	return binary.LittleEndian.Uint32(b[1:5]), b[FrameHeaderBytes:], nil
}

// FrameFromPCM converts little-endian PCM16 bytes to the platform's float32
// frame. The 1/32768 scale makes the conversion bit-exact in both directions
// for every int16 value.
func FrameFromPCM(pcm []byte) (duplex.Frame, error) {
	if len(pcm) != FramePayloadBytes {
		return nil, fmt.Errorf("%w: got %d", ErrBadPayload, len(pcm))
	}
	f := make(duplex.Frame, duplex.FrameSamples)
	for i := range f {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		f[i] = float32(v) / 32768
	}
	return f, nil
}

// PCMFromFrame converts a platform frame to little-endian PCM16 bytes.
func PCMFromFrame(f duplex.Frame) ([]byte, error) {
	if !f.Valid() {
		return nil, fmt.Errorf("%w: got %d samples", ErrBadPayload, len(f))
	}
	pcm := make([]byte, FramePayloadBytes)
	for i, v := range f {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(floatToPCM16(v)))
	}
	return pcm, nil
}

func floatToPCM16(v float32) int16 {
	s := math.Round(float64(v) * 32768)
	if s > math.MaxInt16 {
		return math.MaxInt16
	}
	if s < math.MinInt16 {
		return math.MinInt16
	}
	return int16(s)
}

// Sequencer enforces the per-direction u32 sequence: it starts at 0 and
// increases by exactly one. It is not safe for concurrent use.
type Sequencer struct {
	next    uint32
	started bool
}

// Reset returns the sequencer to its initial state.
func (s *Sequencer) Reset() {
	s.next = 0
	s.started = false
}

// Check consumes seq or returns ErrSequenceGap/ErrSequenceDuplicate.
func (s *Sequencer) Check(seq uint32) error {
	if !s.started {
		if seq != 0 {
			return fmt.Errorf("%w: got %d, want 0", ErrSequenceGap, seq)
		}
		s.started = true
		s.next = 1
		return nil
	}
	if seq < s.next {
		return fmt.Errorf("%w: got %d, want %d", ErrSequenceDuplicate, seq, s.next)
	}
	if seq > s.next {
		return fmt.Errorf("%w: got %d, want %d", ErrSequenceGap, seq, s.next)
	}
	s.next++
	return nil
}

// Next returns the sequence number the next output frame should carry.
func (s *Sequencer) Next() uint32 {
	n := s.next
	s.next++
	s.started = true
	return n
}
