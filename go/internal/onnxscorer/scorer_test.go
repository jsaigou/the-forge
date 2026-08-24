// SPDX-License-Identifier: Apache-2.0

//go:build compressor_integration

// Gated behind the compressor_integration build tag so a plain `go test
// ./...` never needs to link libonnxruntime (see integration_test.go in
// internal/compress for the full rationale). Run with:
//
//	CGO_LDFLAGS="-L<libdir>" ONNXSCORER_TEST_MODEL=<path> \
//	ONNXSCORER_TEST_LIBONNXRUNTIME=<path> \
//	go test -tags compressor_integration ./internal/onnxscorer/... -v
package onnxscorer

import (
	"os"
	"testing"
)

// scorerEnv returns the real kompress-int8-wo.onnx model path and the
// onnxruntime shared library path to test against, skipping if either
// isn't available. This package is cgo-backed and needs both real
// artifacts, so it's exercised locally / on ForgeHost, not in the default `go
// test ./...` CI run — see docs/v5-headroom-replacement.md Sprint 3's
// "Build & deploy" section.
func scorerEnv(t *testing.T) (modelPath, libPath string) {
	t.Helper()
	modelPath = os.Getenv("ONNXSCORER_TEST_MODEL")
	libPath = os.Getenv("ONNXSCORER_TEST_LIBONNXRUNTIME")
	if modelPath == "" || libPath == "" {
		t.Skip("ONNXSCORER_TEST_MODEL / ONNXSCORER_TEST_LIBONNXRUNTIME not set; skipping real-model test")
	}
	return modelPath, libPath
}

func TestScore_RealModel(t *testing.T) {
	modelPath, libPath := scorerEnv(t)
	s, err := New(modelPath, libPath, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// A real ModernBERT tokenization shape: [CLS], some content tokens,
	// [SEP] — ids themselves don't need to be real vocabulary entries to
	// exercise the tensor plumbing and shape contract, only to be
	// in-vocabulary-range int64s the model can embed.
	ids := []int64{50281, 100, 200, 300, 400, 50282}
	mask := []int64{1, 1, 1, 1, 1, 1}

	scores, err := s.Score(ids, mask)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(scores) != len(ids) {
		t.Fatalf("expected %d scores, got %d", len(ids), len(scores))
	}
	for i, sc := range scores {
		if sc < 0 || sc > 1 {
			t.Errorf("score[%d] = %v, want a probability in [0,1]", i, sc)
		}
	}
}

func TestScore_LengthMismatch(t *testing.T) {
	modelPath, libPath := scorerEnv(t)
	s, err := New(modelPath, libPath, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if _, err := s.Score([]int64{1, 2, 3}, []int64{1, 1}); err == nil {
		t.Fatal("expected an error for mismatched input_ids/attention_mask lengths")
	}
}

func TestScore_Concurrent(t *testing.T) {
	modelPath, libPath := scorerEnv(t)
	s, err := New(modelPath, libPath, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ids := []int64{50281, 100, 200, 300, 400, 50282}
	mask := []int64{1, 1, 1, 1, 1, 1}

	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.Score(ids, mask)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Score: %v", err)
		}
	}
}
