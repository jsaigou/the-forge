// SPDX-License-Identifier: Apache-2.0

package gguf

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ggufBuilder assembles a synthetic GGUF file for tests.
type ggufBuilder struct {
	buf         bytes.Buffer
	kvCount     uint64
	tensorCount uint64
	version     uint32
}

func newBuilder() *ggufBuilder { return &ggufBuilder{version: 3} }

func (b *ggufBuilder) w(v any) { _ = binary.Write(&b.buf, binary.LittleEndian, v) }

func (b *ggufBuilder) str(s string) {
	b.w(uint64(len(s)))
	b.buf.WriteString(s)
}

func (b *ggufBuilder) kvString(key, val string) {
	b.str(key)
	b.w(uint32(typeString))
	b.str(val)
	b.kvCount++
}

func (b *ggufBuilder) kvUint32(key string, val uint32) {
	b.str(key)
	b.w(uint32(typeUint32))
	b.w(val)
	b.kvCount++
}

func (b *ggufBuilder) kvUint64(key string, val uint64) {
	b.str(key)
	b.w(uint32(typeUint64))
	b.w(val)
	b.kvCount++
}

func (b *ggufBuilder) kvFloat32(key string, val float32) {
	b.str(key)
	b.w(uint32(typeFloat32))
	b.w(val)
	b.kvCount++
}

func (b *ggufBuilder) kvStringArray(key string, vals ...string) {
	b.str(key)
	b.w(uint32(typeArray))
	b.w(uint32(typeString))
	b.w(uint64(len(vals)))
	for _, v := range vals {
		b.str(v)
	}
	b.kvCount++
}

func (b *ggufBuilder) kvInt32Array(key string, vals ...int32) {
	b.str(key)
	b.w(uint32(typeArray))
	b.w(uint32(typeInt32))
	b.w(uint64(len(vals)))
	for _, v := range vals {
		b.w(v)
	}
	b.kvCount++
}

// build writes the complete file: header, KV section, then `trailer` bytes
// standing in for the tensor table region.
func (b *ggufBuilder) build(t *testing.T, trailer []byte) string {
	t.Helper()
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(ggufMagic))
	_ = binary.Write(&out, binary.LittleEndian, b.version)
	_ = binary.Write(&out, binary.LittleEndian, b.tensorCount)
	_ = binary.Write(&out, binary.LittleEndian, b.kvCount)
	out.Write(b.buf.Bytes())
	out.Write(trailer)

	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadMetadataBasic(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "qwen3")
	b.kvString("general.name", "Qwen3 Test")
	b.kvUint32("general.file_type", 18) // Q6_K
	b.kvUint64("general.parameter_count", 8_190_000_000)
	b.kvUint32("qwen3.context_length", 262144)
	b.kvStringArray("tokenizer.ggml.tokens", "a", "b", "c")
	b.kvInt32Array("tokenizer.ggml.token_type", 1, 2, 3)
	b.kvFloat32("qwen3.rope.freq_base", 1e6)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.Architecture != "qwen3" {
		t.Errorf("Architecture = %q", md.Architecture)
	}
	if md.Name != "Qwen3 Test" {
		t.Errorf("Name = %q", md.Name)
	}
	if md.TrainedCtx != 262144 {
		t.Errorf("TrainedCtx = %d", md.TrainedCtx)
	}
	if md.ParameterCount != 8_190_000_000 {
		t.Errorf("ParameterCount = %d", md.ParameterCount)
	}
	if md.QuantType != "Q6_K" {
		t.Errorf("QuantType = %q", md.QuantType)
	}
	if md.FileSizeBytes <= 0 {
		t.Errorf("FileSizeBytes = %d", md.FileSizeBytes)
	}
}

// TestNeverReadsTensorTable is the crown-jewels test: the file declares a
// large tensor count but contains NOTHING after the KV section. If the
// reader touched the tensor table at all it would hit EOF and error; V4's
// library walked it and cost 62s/call on large models.
func TestNeverReadsTensorTable(t *testing.T) {
	b := newBuilder()
	b.tensorCount = 9999
	b.kvString("general.architecture", "llama")
	b.kvUint32("llama.context_length", 8192)
	path := b.build(t, nil) // zero trailer bytes: tensor table absent entirely

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatalf("reader must not touch the tensor region: %v", err)
	}
	if md.TrainedCtx != 8192 {
		t.Errorf("TrainedCtx = %d", md.TrainedCtx)
	}
}

// TestTrailingGarbageIgnored: bytes after the KV section (the real tensor
// table in production) must never influence or break parsing.
func TestTrailingGarbageIgnored(t *testing.T) {
	b := newBuilder()
	b.tensorCount = 3
	b.kvString("general.architecture", "gemma3")
	b.kvUint32("gemma3.context_length", 131072)
	path := b.build(t, bytes.Repeat([]byte{0xde, 0xad}, 4096))

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.Architecture != "gemma3" || md.TrainedCtx != 131072 {
		t.Errorf("got %+v", md)
	}
}

// Context length may precede general.architecture; key order must not matter.
func TestContextBeforeArch(t *testing.T) {
	b := newBuilder()
	b.kvUint32("nemotron.context_length", 1048576)
	b.kvString("general.architecture", "nemotron")
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.TrainedCtx != 1048576 {
		t.Errorf("TrainedCtx = %d", md.TrainedCtx)
	}
}

// V4 fallback: llama.context_length is honored when the declared arch has no
// matching <arch>.context_length key.
func TestLlamaContextFallback(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "weirdarch")
	b.kvUint32("llama.context_length", 4096)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.TrainedCtx != 4096 {
		t.Errorf("TrainedCtx = %d", md.TrainedCtx)
	}
}

// Context length as uint64 (some converters) must parse too.
func TestUint64Context(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "glm4")
	b.kvUint64("glm4.context_length", 131072)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.TrainedCtx != 131072 {
		t.Errorf("TrainedCtx = %d", md.TrainedCtx)
	}
}

func TestRejectsNonGGUF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.gguf")
	if err := os.WriteFile(path, []byte("definitely not a gguf file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("expected error for non-GGUF file")
	}
}

func TestRejectsV1(t *testing.T) {
	b := newBuilder()
	b.version = 1
	b.kvString("general.architecture", "llama")
	path := b.build(t, nil)
	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("expected error for GGUF v1")
	}
}

func TestRejectsMissingFile(t *testing.T) {
	if _, err := ReadMetadata(filepath.Join(t.TempDir(), "absent.gguf")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRejectsTruncatedKV(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "llama")
	b.kvUint32("llama.context_length", 8192)
	path := b.build(t, nil)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trunc := filepath.Join(t.TempDir(), "trunc.gguf")
	if err := os.WriteFile(trunc, raw[:len(raw)-6], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMetadata(trunc); err == nil {
		t.Fatal("expected error for truncated KV section")
	}
}

// Corrupt declared lengths must fail fast, not hang or allocate wildly.
func TestRejectsImplausibleLengths(t *testing.T) {
	b := newBuilder()
	b.str("general.architecture")
	b.w(uint32(typeString))
	b.w(uint64(1 << 60)) // absurd string length
	b.kvCount++
	path := b.build(t, nil)
	if _, err := ReadMetadata(path); err == nil {
		t.Fatal("expected error for implausible string length")
	}
}
