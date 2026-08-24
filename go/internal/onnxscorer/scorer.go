// SPDX-License-Identifier: Apache-2.0

// Package onnxscorer wraps yalue/onnxruntime_go (a cgo binding that dlopen's
// libonnxruntime.so/.dylib at a configured path — never statically linked)
// to satisfy compress.Scorer with the real chopratejas/kompress-v2-base
// ONNX model.
//
// Isolated in its own package specifically so forge's own pure-Go build
// never imports it — see docs/v5-headroom-replacement.md Sprint 3's "Build
// & deploy" section.
//
// Verified live on ForgeHost (Sprint 3 planning): the model's exact I/O
// contract is int64 [batch,seq] input_ids/attention_mask in, float
// [batch,seq] final_scores out, keep-decision simply score > threshold
// (KompressConfig.score_threshold, default 0.5 — kompress_compressor.py's
// _OnnxModel.get_keep_mask).
package onnxscorer

import (
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/jsaigou/the-forge/internal/compress"
)

var (
	envOnce sync.Once
	envErr  error
)

// InitEnvironment points onnxruntime_go at the shared library and brings up
// the (process-global — the underlying C API only supports one per
// process) ONNX Runtime environment. Safe to call more than once; only the
// first call's sharedLibPath takes effect. Must be called (directly, or via
// New's own call) before any Scorer is created.
func InitEnvironment(sharedLibPath string) error {
	envOnce.Do(func() {
		ort.SetSharedLibraryPath(sharedLibPath)
		envErr = ort.InitializeEnvironment()
	})
	return envErr
}

// Scorer wraps one loaded ONNX session for the Kompress model. A single
// Scorer is safe for concurrent Score calls: ONNX Runtime's own concurrency
// contract is that Run() on one session is thread-safe across goroutines
// as long as each call uses its own input/output tensors (which Score
// does, allocating fresh ones per call and never sharing them across
// calls) — no additional locking needed, deliberately unlike headroom-ai's
// Python wrapper, whose _execution_semaphore existed for GIL/async
// scheduling reasons that don't apply here, not because ORT itself
// serializes Run().
type Scorer struct {
	session *ort.DynamicAdvancedSession
}

var _ compress.Scorer = (*Scorer)(nil)

// New loads the ONNX model at modelPath and creates a session ready for
// concurrent Score calls. sharedLibPath is the path to
// libonnxruntime.so/.dylib; InitEnvironment must have been called (or is
// called here) with it before any session is created. intraOpThreads sets
// SessionOptions.SetIntraOpNumThreads (0 uses onnxruntime's own default);
// pass the same value every hand-created headroom-ai instance on ForgeHost
// already used (internal/compressorctl.kompressIntraThreads, 16) to match its
// resource footprint.
func New(modelPath, sharedLibPath string, intraOpThreads int) (*Scorer, error) {
	if err := InitEnvironment(sharedLibPath); err != nil {
		return nil, fmt.Errorf("onnxscorer: init environment: %w", err)
	}

	var opts *ort.SessionOptions
	if intraOpThreads > 0 {
		so, err := ort.NewSessionOptions()
		if err != nil {
			return nil, fmt.Errorf("onnxscorer: session options: %w", err)
		}
		defer so.Destroy()
		if err := so.SetIntraOpNumThreads(intraOpThreads); err != nil {
			return nil, fmt.Errorf("onnxscorer: set intra-op threads: %w", err)
		}
		opts = so
	}

	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"input_ids", "attention_mask"},
		[]string{"final_scores"},
		opts,
	)
	if err != nil {
		return nil, fmt.Errorf("onnxscorer: load model %s: %w", modelPath, err)
	}
	return &Scorer{session: session}, nil
}

// Close releases the ONNX session. It does not tear down the process-global
// environment (matching onnxruntime_go's own singleton contract).
func (s *Scorer) Close() error {
	return s.session.Destroy()
}

// Score implements compress.Scorer: one batch-of-1 forward pass over
// inputIDs/attentionMask, returning one score per input position.
func (s *Scorer) Score(inputIDs, attentionMask []int64) ([]float32, error) {
	if len(inputIDs) != len(attentionMask) {
		return nil, fmt.Errorf("onnxscorer: input_ids length %d != attention_mask length %d", len(inputIDs), len(attentionMask))
	}
	if len(inputIDs) == 0 {
		return nil, nil
	}

	shape := ort.NewShape(1, int64(len(inputIDs)))
	idsTensor, err := ort.NewTensor(shape, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("onnxscorer: build input_ids tensor: %w", err)
	}
	defer idsTensor.Destroy()

	maskTensor, err := ort.NewTensor(shape, attentionMask)
	if err != nil {
		return nil, fmt.Errorf("onnxscorer: build attention_mask tensor: %w", err)
	}
	defer maskTensor.Destroy()

	outputs := []ort.Value{nil}
	if err := s.session.Run([]ort.Value{idsTensor, maskTensor}, outputs); err != nil {
		return nil, fmt.Errorf("onnxscorer: run: %w", err)
	}
	out, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		outputs[0].Destroy()
		return nil, fmt.Errorf("onnxscorer: final_scores output is %T, want *Tensor[float32]", outputs[0])
	}
	defer out.Destroy()

	data := out.GetData()
	scores := make([]float32, len(data))
	copy(scores, data)
	return scores, nil
}
