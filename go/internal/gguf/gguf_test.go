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

func (b *ggufBuilder) kvBoolArray(key string, vals ...bool) {
	b.str(key)
	b.w(uint32(typeArray))
	b.w(uint32(typeBool))
	b.w(uint64(len(vals)))
	for _, v := range vals {
		bb := uint8(0)
		if v {
			bb = 1
		}
		b.w(bb)
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

// A plain dense/GQA model: scalar head_count_kv, no per-layer SWA split —
// the common case the KV-cache estimator (engine/kvcache.go) must also
// handle, not just Gemma's per-layer arrays.
func TestReadMetadataPlainGQA(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "llama")
	b.kvUint32("llama.block_count", 32)
	b.kvUint32("llama.embedding_length", 4096)
	b.kvUint32("llama.attention.head_count", 32)
	b.kvUint32("llama.attention.head_count_kv", 8) // scalar
	b.kvUint32("llama.attention.key_length", 128)
	b.kvUint32("llama.attention.value_length", 128)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.BlockCount != 32 {
		t.Errorf("BlockCount = %d, want 32", md.BlockCount)
	}
	if len(md.HeadCountKV) != 32 {
		t.Fatalf("HeadCountKV len = %d, want 32 (scalar broadcast)", len(md.HeadCountKV))
	}
	for i, v := range md.HeadCountKV {
		if v != 8 {
			t.Fatalf("HeadCountKV[%d] = %d, want 8", i, v)
		}
	}
	if md.KeyLength != 128 || md.ValueLength != 128 {
		t.Errorf("KeyLength/ValueLength = %d/%d, want 128/128", md.KeyLength, md.ValueLength)
	}
	if md.SWAPattern != nil {
		t.Errorf("SWAPattern = %v, want nil (no per-layer split declared)", md.SWAPattern)
	}
	if md.Hybrid {
		t.Error("Hybrid = true, want false")
	}
}

// Gemma4-shaped model: per-layer head_count_kv and sliding_window_pattern
// arrays, plus separate SWA key/value lengths — the exact shape ground-
// truthed against the real gemma4-26b-a4b GGUF on ForgeHost during the
// 2026-09-07 incident investigation (30 layers, 5 SWA : 1 global pattern).
func TestReadMetadataGemmaISWA(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "gemma4")
	b.kvUint32("gemma4.block_count", 6) // small stand-in for the real 30
	b.kvUint32("gemma4.embedding_length", 2816)
	b.kvUint32("gemma4.attention.head_count", 16)
	b.kvInt32Array("gemma4.attention.head_count_kv", 8, 8, 8, 8, 8, 2)
	b.kvUint32("gemma4.attention.key_length", 512)
	b.kvUint32("gemma4.attention.value_length", 512)
	b.kvUint32("gemma4.attention.key_length_swa", 256)
	b.kvUint32("gemma4.attention.value_length_swa", 256)
	b.kvUint32("gemma4.attention.sliding_window", 1024)
	b.kvBoolArray("gemma4.attention.sliding_window_pattern", true, true, true, true, true, false)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.BlockCount != 6 {
		t.Fatalf("BlockCount = %d, want 6", md.BlockCount)
	}
	wantKV := []int{8, 8, 8, 8, 8, 2}
	if len(md.HeadCountKV) != 6 {
		t.Fatalf("HeadCountKV len = %d, want 6", len(md.HeadCountKV))
	}
	for i, v := range wantKV {
		if md.HeadCountKV[i] != v {
			t.Errorf("HeadCountKV[%d] = %d, want %d", i, md.HeadCountKV[i], v)
		}
	}
	wantPattern := []bool{true, true, true, true, true, false}
	if len(md.SWAPattern) != 6 {
		t.Fatalf("SWAPattern len = %d, want 6", len(md.SWAPattern))
	}
	for i, v := range wantPattern {
		if md.SWAPattern[i] != v {
			t.Errorf("SWAPattern[%d] = %v, want %v", i, md.SWAPattern[i], v)
		}
	}
	if md.KeyLengthSWA != 256 || md.ValueLengthSWA != 256 {
		t.Errorf("KeyLengthSWA/ValueLengthSWA = %d/%d, want 256/256", md.KeyLengthSWA, md.ValueLengthSWA)
	}
	if md.SlidingWindow != 1024 {
		t.Errorf("SlidingWindow = %d, want 1024", md.SlidingWindow)
	}
}

// Presence of an ssm.* key must set Hybrid, regardless of its value —
// qwen4exp (Qwen3.8-Flash-Next) declares ssm.state_size etc. alongside
// ordinary attention keys, and the KV-cache formula must abstain rather
// than apply dense-attention math to a recurrent-state architecture.
func TestReadMetadataHybridSSMSignal(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "qwen4exp")
	b.kvUint32("qwen4exp.block_count", 48)
	b.kvUint32("qwen4exp.ssm.state_size", 128)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if !md.Hybrid {
		t.Error("Hybrid = false, want true (ssm.* key present)")
	}
}

// Presence of an attention.indexer.* key (the block-sparse indexer cache
// qwen4exp also carries) must likewise set Hybrid.
func TestReadMetadataHybridIndexerSignal(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "qwen4exp")
	b.kvUint32("qwen4exp.attention.indexer.head_count", 4)
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if !md.Hybrid {
		t.Error("Hybrid = false, want true (attention.indexer.* key present)")
	}
}

// A head_count_kv array whose length doesn't match block_count is corrupt
// or unparseable in a way we can't trust — must be dropped, not guessed at.
func TestReadMetadataHeadCountKVLengthMismatchDropped(t *testing.T) {
	b := newBuilder()
	b.kvString("general.architecture", "weird")
	b.kvUint32("weird.block_count", 4)
	b.kvInt32Array("weird.attention.head_count_kv", 8, 8) // len 2, want 4 or 1
	path := b.build(t, nil)

	md, err := ReadMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if md.HeadCountKV != nil {
		t.Errorf("HeadCountKV = %v, want nil (length mismatch must not be guessed)", md.HeadCountKV)
	}
}
