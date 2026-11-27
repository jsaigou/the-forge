// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// A vLLM-kind engine takes a build with no binary whatever its backend; a llama.cpp-kind engine still needs one; an engine row with a kind
// this binary does not know takes no builds at all (ADR-0025).
func TestBuildWrites_BinaryRuleFollowsEngineKind(t *testing.T) {
	ctx := context.Background()
	w, cat, _ := newBuildWrites()
	cat.engines = []store.Engine{
		{ID: 1, Name: "llama.cpp", Kind: "llamacpp"},
		{ID: 2, Name: "vLLM", Kind: "vllm"},
		{ID: 3, Name: "mystery", Kind: "bogus"},
	}
	if _, err := w.Create(ctx, store.Build{EngineID: 2, Name: "vllm-rocm", Backend: "rocm"}); err != nil {
		t.Errorf("a vllm-kind build with GPU backend rocm needs no binary: %v", err)
	}
	if _, err := w.Create(ctx, store.Build{EngineID: 1, Name: "llama-rocm", Backend: "rocm"}); err == nil {
		t.Error("a llamacpp build without a binary was accepted")
	}
	if _, err := w.Create(ctx, store.Build{EngineID: 3, Name: "m", Backend: "rocm", BinaryPath: "/opt/x/bin"}); err == nil {
		t.Error("a build on an engine of unknown kind was accepted")
	}
}
