package duplexremote

import (
	"encoding/binary"
	"testing"
)

func TestSoakWavBytesIsACanonicalHeaderPlusSamples(t *testing.T) {
	b := wavBytes([]int16{1, -2, 300})
	if len(b) != 44+6 || string(b[0:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" {
		t.Fatalf("bad header or length %d", len(b))
	}
	le := binary.LittleEndian
	if le.Uint32(b[4:8]) != 36+6 || le.Uint16(b[20:22]) != 1 || le.Uint16(b[22:24]) != 1 || le.Uint32(b[24:28]) != 24000 || le.Uint32(b[28:32]) != 48000 || le.Uint16(b[34:36]) != 16 || le.Uint32(b[40:44]) != 6 {
		t.Fatalf("fields wrong: % x", b[:44])
	}
	if int16(le.Uint16(b[44:])) != 1 || int16(le.Uint16(b[46:])) != -2 || int16(le.Uint16(b[48:])) != 300 {
		t.Fatalf("samples wrong: % x", b[44:])
	}
}
