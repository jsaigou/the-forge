// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParsePromSum(t *testing.T) {
	text := `# HELP compress_cache_read_tokens_total Total cache-read tokens
# TYPE compress_cache_read_tokens_total counter
compress_cache_read_tokens_total{provider="openai"} 33
compress_cache_read_tokens_total{provider="anthropic"} 12
compress_cache_read_tokens_total_bucket{le="10"} 999
compress_tokens_input_total 18052
`
	sum, series, ok := parsePromSum(text, "compress_cache_read_tokens_total")
	if !ok || series != 2 || sum != 45 {
		t.Errorf("parsePromSum(cache_read) = sum=%v series=%v ok=%v, want 45 2 true", sum, series, ok)
	}

	sum, series, ok = parsePromSum(text, "compress_tokens_input_total")
	if !ok || series != 1 || sum != 18052 {
		t.Errorf("parsePromSum(tokens_input) = sum=%v series=%v ok=%v, want 18052 1 true", sum, series, ok)
	}

	_, _, ok = parsePromSum(text, "compress_nonexistent_total")
	if ok {
		t.Error("parsePromSum on a missing metric must return ok=false")
	}
}

func TestParsePromSumIgnoresBucketCollision(t *testing.T) {
	// A metric name that is a prefix of another metric's name must not be
	// double-counted via the bucket variant.
	text := "foo_total 5\nfoo_total_bucket{le=\"1\"} 999\n"
	sum, series, ok := parsePromSum(text, "foo_total")
	if !ok || series != 1 || sum != 5 {
		t.Errorf("parsePromSum(foo_total) = sum=%v series=%v ok=%v, want 5 1 true (must not match foo_total_bucket)", sum, series, ok)
	}
}

func TestParsePromByLabel(t *testing.T) {
	text := `compress_requests_by_model{model="/opt/forge/models/laguna-s-2.1-Q4_K_M.gguf"} 811
compress_requests_by_model{model="/opt/forge/models/gpt-oss-120b.gguf"} 42
compress_requests_by_provider{provider="openai"} 853
`
	byModel := parsePromByLabel(text, "compress_requests_by_model", "model")
	want := map[string]float64{
		"/opt/forge/models/laguna-s-2.1-Q4_K_M.gguf": 811,
		"/opt/forge/models/gpt-oss-120b.gguf":        42,
	}
	if !reflect.DeepEqual(byModel, want) {
		t.Errorf("parsePromByLabel(model) = %v, want %v", byModel, want)
	}

	byProvider := parsePromByLabel(text, "compress_requests_by_provider", "provider")
	if want := (map[string]float64{"openai": 853}); !reflect.DeepEqual(byProvider, want) {
		t.Errorf("parsePromByLabel(provider) = %v, want %v", byProvider, want)
	}
}

func TestParsePromByLabelSumsRepeatedValues(t *testing.T) {
	// Two series that happen to share the requested label's value (e.g.
	// distinguished only by a second label the caller isn't reading) must
	// sum rather than overwrite.
	text := `compress_cache_write_ttl_tokens_total{provider="openai",ttl="1h"} 10
compress_cache_write_ttl_tokens_total{provider="openai",ttl="5m"} 7
`
	byProvider := parsePromByLabel(text, "compress_cache_write_ttl_tokens_total", "provider")
	if want := (map[string]float64{"openai": 17}); !reflect.DeepEqual(byProvider, want) {
		t.Errorf("parsePromByLabel sum = %v, want %v", byProvider, want)
	}
}

func TestParsePromByLabelMalformedOrMissingLabel(t *testing.T) {
	text := `compress_requests_by_model{modelx="x"} 5
compress_requests_by_model{model=} 6
compress_requests_by_model{model="ok"} 7
compress_requests_by_model missing_braces 8
`
	got := parsePromByLabel(text, "compress_requests_by_model", "model")
	// Only the well-formed "ok" series should survive: the first line lacks
	// the requested label, the second has an empty (unparseable-as-KV — no
	// quoted value) assignment that still parses as label "model" = "" per
	// the naive splitter, and the last has no braces at all.
	if v, ok := got["ok"]; !ok || v != 7 {
		t.Errorf("parsePromByLabel malformed input = %v, want a well-formed \"ok\": 7 entry", got)
	}
	if _, present := got["x"]; present {
		t.Errorf("parsePromByLabel must not attribute a series missing the requested label: %v", got)
	}
}

func TestParsePromScalarStillIgnoresComments(t *testing.T) {
	// Regression guard: parsePromSum/parsePromByLabel must skip comment
	// lines the same way parsePromScalar does.
	text := "# compress_tokens_input_total 999999\ncompress_tokens_input_total 5\n"
	sum, series, ok := parsePromSum(text, "compress_tokens_input_total")
	if !ok || series != 1 || sum != 5 {
		t.Errorf("parsePromSum with a comment line = sum=%v series=%v ok=%v, want 5 1 true", sum, series, ok)
	}
}

func TestMetricsParsesLiveDecodeCounters(t *testing.T) {
	// llama.cpp #26920-era builds: the TPS gauges and token totals are
	// frozen during a long generation, but n_decode_total / n_tokens_max
	// advance every decode batch. The collector must capture both.
	text := `llamacpp:requests_processing 1
llamacpp:prompt_tokens_seconds 0
llamacpp:predicted_tokens_seconds 0
llamacpp:prompt_tokens_total 503733
llamacpp:tokens_predicted_total 38690
llamacpp:prompt_seconds_total 4508.19
llamacpp:n_decode_total 14210
llamacpp:n_tokens_max 231419
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, text)
	}))
	defer srv.Close()

	m := NewLlamaClient(func(port int) string { return srv.URL }).Metrics(context.Background(), 8080)
	if m == nil {
		t.Fatal("Metrics returned nil")
	}
	if m.RequestsProcessing != 1 || m.PromptTPS != 0 || m.PredictedTPS != 0 {
		t.Fatalf("scalar gauges = %v/%v/%v, want 1/0/0", m.RequestsProcessing, m.PromptTPS, m.PredictedTPS)
	}
	if m.NDecodeTotal == nil || *m.NDecodeTotal != 14210 {
		t.Fatalf("NDecodeTotal = %v, want 14210", m.NDecodeTotal)
	}
	if m.NTokensMax == nil || *m.NTokensMax != 231419 {
		t.Fatalf("NTokensMax = %v, want 231419", m.NTokensMax)
	}
	if m.PromptTotal == nil || m.PredictedTotal == nil {
		t.Fatal("token totals must parse")
	}
}
