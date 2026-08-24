// SPDX-License-Identifier: Apache-2.0

// Package gguf reads GGUF model metadata. Owned by track A (Phase 2), which
// ports V4's read_model_metadata() semantics.
//
// HARD REQUIREMENT (docs/v5-plan.md Phase 2): parse the header and KV
// section ONLY — never walk the tensor table. Reading the tensor table cost
// 62s/call in the Python implementation (progress.md). The reader below is
// strictly sequential over the header + KV pairs and stops dead the moment
// the last KV pair has been consumed; nothing after that offset is read.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Metadata is the KV subset the engine and registry need (Contract 2).
type Metadata struct {
	Architecture string
	Name         string
	// TrainedCtx is <arch>.context_length — compared against configured
	// and actual n_ctx (crown jewels: silent context reduction).
	TrainedCtx     int
	ParameterCount int64
	QuantType      string
	FileSizeBytes  int64
}

const ggufMagic = 0x46554747 // "GGUF" little-endian

// GGUF metadata value types (gguf_metadata_value_type in the spec).
const (
	typeUint8   = 0
	typeInt8    = 1
	typeUint16  = 2
	typeInt16   = 3
	typeUint32  = 4
	typeInt32   = 5
	typeFloat32 = 6
	typeBool    = 7
	typeString  = 8
	typeArray   = 9
	typeUint64  = 10
	typeInt64   = 11
	typeFloat64 = 12
)

var scalarSize = map[uint32]int64{
	typeUint8: 1, typeInt8: 1, typeBool: 1,
	typeUint16: 2, typeInt16: 2,
	typeUint32: 4, typeInt32: 4, typeFloat32: 4,
	typeUint64: 8, typeInt64: 8, typeFloat64: 8,
}

// fileType values from llama.cpp's llama_ftype enum (general.file_type).
var fileTypeNames = map[uint64]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1",
	7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
	10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L",
	14: "Q4_K_S", 15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M",
	18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S",
	22: "IQ3_XS", 23: "IQ3_XXS", 24: "IQ1_S", 25: "IQ4_NL",
	26: "IQ3_S", 27: "IQ3_M", 28: "IQ2_S", 29: "IQ2_M",
	30: "IQ4_XS", 31: "IQ1_M", 32: "BF16",
}

// maxKVCount bounds the KV loop so a corrupt header cannot spin the reader;
// real models carry a few hundred pairs (tokenizer arrays are single pairs).
const maxKVCount = 1 << 20

// maxKeptString bounds strings we retain (arch/name); huge values for kept
// keys indicate corruption, not a real model card.
const maxKeptString = 1 << 16

// ReadMetadata parses path's GGUF header + KV pairs. It returns an error for
// missing/unreadable/non-GGUF files (V4 returned {} — callers decide whether
// absence is fatal; the Go port surfaces why).
func ReadMetadata(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("gguf: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return Metadata{}, fmt.Errorf("gguf: %w", err)
	}

	md, err := readAll(bufio.NewReaderSize(f, 1<<16), st.Size())
	if err != nil {
		return Metadata{}, fmt.Errorf("gguf %s: %w", path, err)
	}
	md.FileSizeBytes = st.Size()
	return md, nil
}

// readAll consumes the header and every KV pair from r, keeping only the
// fields Metadata needs. fileSize bounds declared lengths so corrupt counts
// fail fast instead of spinning on io.Discard.
func readAll(r *bufio.Reader, fileSize int64) (Metadata, error) {
	var md Metadata

	var magic, version uint32
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return md, fmt.Errorf("reading magic: %w", err)
	}
	if magic != ggufMagic {
		return md, errors.New("not a GGUF file")
	}
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return md, fmt.Errorf("reading version: %w", err)
	}
	if version < 2 || version > 3 {
		return md, fmt.Errorf("unsupported GGUF version %d", version)
	}

	var tensorCount, kvCount uint64
	if err := binary.Read(r, binary.LittleEndian, &tensorCount); err != nil {
		return md, fmt.Errorf("reading tensor count: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &kvCount); err != nil {
		return md, fmt.Errorf("reading kv count: %w", err)
	}
	if kvCount > maxKVCount {
		return md, fmt.Errorf("implausible kv count %d", kvCount)
	}

	// ctxByArch holds every *.context_length seen, so key order relative to
	// general.architecture doesn't matter.
	ctxByArch := map[string]uint64{}
	var fileType uint64
	var haveFileType bool

	for i := uint64(0); i < kvCount; i++ {
		key, err := readString(r, fileSize)
		if err != nil {
			return md, fmt.Errorf("kv %d key: %w", i, err)
		}
		var vt uint32
		if err := binary.Read(r, binary.LittleEndian, &vt); err != nil {
			return md, fmt.Errorf("kv %q type: %w", key, err)
		}

		switch {
		case key == "general.architecture" || key == "general.name":
			s, err := readTypedString(r, vt, fileSize)
			if err != nil {
				return md, fmt.Errorf("kv %q: %w", key, err)
			}
			if key == "general.architecture" {
				md.Architecture = s
			} else {
				md.Name = s
			}
		case key == "general.file_type":
			u, err := readTypedUint(r, vt)
			if err != nil {
				return md, fmt.Errorf("kv %q: %w", key, err)
			}
			fileType, haveFileType = u, true
		case key == "general.parameter_count":
			u, err := readTypedUint(r, vt)
			if err != nil {
				return md, fmt.Errorf("kv %q: %w", key, err)
			}
			md.ParameterCount = int64(u)
		case strings.HasSuffix(key, ".context_length"):
			u, err := readTypedUint(r, vt)
			if err != nil {
				return md, fmt.Errorf("kv %q: %w", key, err)
			}
			ctxByArch[strings.TrimSuffix(key, ".context_length")] = u
		default:
			if err := skipValue(r, vt, fileSize); err != nil {
				return md, fmt.Errorf("kv %q: %w", key, err)
			}
		}
	}
	// KV section fully consumed — STOP. The tensor table begins here and is
	// deliberately never read (the 62s/call mistake).

	if ctx, ok := ctxByArch[md.Architecture]; ok {
		md.TrainedCtx = int(ctx)
	} else if ctx, ok := ctxByArch["llama"]; ok {
		// V4 fallback: some converters emit llama.context_length regardless
		// of the declared architecture.
		md.TrainedCtx = int(ctx)
	}
	if haveFileType {
		if name, ok := fileTypeNames[fileType]; ok {
			md.QuantType = name
		} else {
			md.QuantType = fmt.Sprintf("unknown(%d)", fileType)
		}
	}
	return md, nil
}

func readString(r *bufio.Reader, fileSize int64) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if int64(n) < 0 || int64(n) > fileSize {
		return "", fmt.Errorf("string length %d exceeds file size", n)
	}
	if n > maxKeptString {
		// Too big to be a key or a kept value; consume without retaining.
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return "", err
		}
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readTypedString(r *bufio.Reader, vt uint32, fileSize int64) (string, error) {
	if vt != typeString {
		return "", fmt.Errorf("expected string, got type %d", vt)
	}
	return readString(r, fileSize)
}

// readTypedUint reads any integer-typed scalar and returns it widened to
// uint64 (context lengths have been seen as uint32 and uint64 across
// converters; V4's reader was similarly permissive).
func readTypedUint(r *bufio.Reader, vt uint32) (uint64, error) {
	switch vt {
	case typeUint8:
		var v uint8
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeInt8:
		var v int8
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeUint16:
		var v uint16
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeInt16:
		var v int16
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeUint32:
		var v uint32
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeInt32:
		var v int32
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeUint64:
		var v uint64
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case typeInt64:
		var v int64
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeFloat32:
		var v float32
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case typeFloat64:
		var v float64
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	default:
		return 0, fmt.Errorf("expected numeric scalar, got type %d", vt)
	}
}

// skipValue consumes one value of type vt without retaining it.
func skipValue(r *bufio.Reader, vt uint32, fileSize int64) error {
	if size, ok := scalarSize[vt]; ok {
		_, err := io.CopyN(io.Discard, r, size)
		return err
	}
	switch vt {
	case typeString:
		_, err := readStringLenSkip(r, fileSize)
		return err
	case typeArray:
		var elemType uint32
		if err := binary.Read(r, binary.LittleEndian, &elemType); err != nil {
			return err
		}
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return err
		}
		if size, ok := scalarSize[elemType]; ok {
			total := int64(count) * size
			if total < 0 || total > fileSize {
				return fmt.Errorf("array of %d×%dB exceeds file size", count, size)
			}
			_, err := io.CopyN(io.Discard, r, total)
			return err
		}
		switch elemType {
		case typeString:
			// Tokenizer vocabularies land here: skip element by element.
			if int64(count) < 0 || int64(count) > fileSize {
				return fmt.Errorf("implausible string array count %d", count)
			}
			for i := uint64(0); i < count; i++ {
				if _, err := readStringLenSkip(r, fileSize); err != nil {
					return err
				}
			}
			return nil
		case typeArray:
			if int64(count) < 0 || int64(count) > fileSize {
				return fmt.Errorf("implausible nested array count %d", count)
			}
			for i := uint64(0); i < count; i++ {
				if err := skipValue(r, typeArray, fileSize); err != nil {
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("unknown array element type %d", elemType)
		}
	default:
		return fmt.Errorf("unknown value type %d", vt)
	}
}

// readStringLenSkip consumes one length-prefixed string, discarding its
// bytes, and returns the length consumed.
func readStringLenSkip(r *bufio.Reader, fileSize int64) (int64, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return 0, err
	}
	if int64(n) < 0 || int64(n) > fileSize {
		return 0, fmt.Errorf("string length %d exceeds file size", n)
	}
	if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
		return 0, err
	}
	return int64(n), nil
}
