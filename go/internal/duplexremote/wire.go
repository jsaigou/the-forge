// SPDX-License-Identifier: Apache-2.0

// Package duplexremote is the Forge side of docs/duplex/engine-contract.md: a
// duplex.DuplexTenant that runs a lane on an out-of-process model engine over
// a loopback WebSocket, plus a mock engine that implements the engine side of
// the same contract so the seam can be tested without any model.
package duplexremote

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/jsaigou/the-forge/internal/duplex"
	"golang.org/x/net/websocket"
)

// ContractVersion is the only engine-contract version this package speaks.
const ContractVersion = "0"

const (
	pathInfo    = "/duplex/v1/info"
	pathSession = "/duplex/v1/session"

	flagAudio = 1 << 0
	flagText  = 1 << 1

	stepInSize     = 4 + duplex.FrameSamples*4
	stepOutHeader  = 4 + 1 + 4
	maxControlSize = 1 << 20 // card + reference metadata; audio is a separate binary message
)

// MaxReferenceChunk is the largest binary message the reference audio is split
// into. A real engine's WebSocket transport closes the connection on a binary
// message of about 3 MiB or more (measured on Radiance's /v1/realtime and
// duplex routes alike), so a reference of any size travels as several messages
// of at most this size that add up to reference.bytes (engine-contract.md §5.1).
const MaxReferenceChunk = 1 << 20

// wsMsg is one WebSocket message with its frame type. x/net/websocket's stock
// codecs discard the payload type; the contract distinguishes JSON control
// (text) from audio (binary), so this codec keeps it.
type wsMsg struct {
	Text bool
	Data []byte
}

var msgCodec = websocket.Codec{
	Marshal: func(v interface{}) ([]byte, byte, error) {
		m := v.(wsMsg)
		if m.Text {
			return m.Data, websocket.TextFrame, nil
		}
		return m.Data, websocket.BinaryFrame, nil
	},
	Unmarshal: func(data []byte, payloadType byte, v interface{}) error {
		m := v.(*wsMsg)
		m.Text = payloadType == websocket.TextFrame
		m.Data = data
		return nil
	},
}

func sendText(ws *websocket.Conn, b []byte) error {
	return msgCodec.Send(ws, wsMsg{Text: true, Data: b})
}

func sendBinary(ws *websocket.Conn, b []byte) error {
	return msgCodec.Send(ws, wsMsg{Data: b})
}

// sendReference sends reference audio as one or more binary messages of at most
// MaxReferenceChunk bytes, in order. A reference that fits is one message.
func sendReference(ws *websocket.Conn, ref []byte) error {
	for len(ref) > 0 {
		n := len(ref)
		if n > MaxReferenceChunk {
			n = MaxReferenceChunk
		}
		if err := sendBinary(ws, ref[:n]); err != nil {
			return err
		}
		ref = ref[n:]
	}
	return nil
}

func recvMsg(ws *websocket.Conn) (wsMsg, error) {
	var m wsMsg
	err := msgCodec.Receive(ws, &m)
	return m, err
}

func encodeStepIn(seq uint32, f duplex.Frame) []byte {
	b := make([]byte, stepInSize)
	binary.LittleEndian.PutUint32(b, seq)
	putFloats(b[4:], f)
	return b
}

func decodeStepIn(b []byte) (uint32, duplex.Frame, error) {
	if len(b) != stepInSize {
		return 0, nil, fmt.Errorf("step input is %d bytes, want %d", len(b), stepInSize)
	}
	f := make(duplex.Frame, duplex.FrameSamples)
	getFloats(f, b[4:])
	return binary.LittleEndian.Uint32(b), f, nil
}

func encodeStepOut(seq uint32, audio []float32, text string) []byte {
	var flags byte
	size := stepOutHeader + len(text)
	if len(audio) > 0 {
		flags |= flagAudio
		size += duplex.FrameSamples * 4
	}
	if text != "" {
		flags |= flagText
	}
	b := make([]byte, size)
	binary.LittleEndian.PutUint32(b, seq)
	b[4] = flags
	binary.LittleEndian.PutUint32(b[5:], uint32(len(text)))
	copy(b[stepOutHeader:], text)
	if len(audio) > 0 {
		putFloats(b[stepOutHeader+len(text):], audio)
	}
	return b
}

var errShortStep = errors.New("step reply is truncated")

func decodeStepOut(b []byte) (seq uint32, audio []float32, text string, err error) {
	if len(b) < stepOutHeader {
		return 0, nil, "", errShortStep
	}
	seq = binary.LittleEndian.Uint32(b)
	flags := b[4]
	tl := int(binary.LittleEndian.Uint32(b[5:]))
	if flags&flagText == 0 && tl != 0 {
		return 0, nil, "", errors.New("step reply has text bytes but no text flag")
	}
	rest := b[stepOutHeader:]
	if tl < 0 || tl > len(rest) {
		return 0, nil, "", errShortStep
	}
	text = string(rest[:tl])
	rest = rest[tl:]
	if flags&flagAudio != 0 {
		if len(rest) != duplex.FrameSamples*4 {
			return 0, nil, "", fmt.Errorf("step reply audio is %d bytes, want %d", len(rest), duplex.FrameSamples*4)
		}
		audio = make([]float32, duplex.FrameSamples)
		getFloats(audio, rest)
	} else if len(rest) != 0 {
		return 0, nil, "", errors.New("step reply has trailing bytes")
	}
	return seq, audio, text, nil
}

func putFloats(dst []byte, src []float32) {
	for i, v := range src {
		binary.LittleEndian.PutUint32(dst[i*4:], math.Float32bits(v))
	}
}

func getFloats(dst []float32, src []byte) {
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(src[i*4:]))
	}
}
