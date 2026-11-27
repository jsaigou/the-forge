// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"fmt"
)

// ModelsInfo is what an OpenAI-compatible /v1/models says about the one model a slot serves: the context bound the engine was started with and its
// capability flags. Radiance reports max_model_len as the REQUESTED --max-model-len (it does not negotiate), so it proves the flag took effect, not
// that memory exists for it; see Gauge and enginekind.Spec.ContextCapacitySeries.
type ModelsInfo struct {
	ID          string
	MaxModelLen int
	Caps        map[string]bool // boolean members of "capabilities"; non-boolean members are dropped
}

// ModelsInfo queries /v1/models and returns the first model entry.
func (l *LlamaClient) ModelsInfo(ctx context.Context, port int) (ModelsInfo, error) {
	raw, err := l.get(ctx, port, "/v1/models")
	if err != nil {
		return ModelsInfo{}, err
	}
	var body struct {
		Data []struct {
			ID           string                     `json:"id"`
			MaxModelLen  int                        `json:"max_model_len"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ModelsInfo{}, err
	}
	if len(body.Data) == 0 {
		return ModelsInfo{}, fmt.Errorf("/v1/models lists no model")
	}
	d := body.Data[0]
	info := ModelsInfo{ID: d.ID, MaxModelLen: d.MaxModelLen, Caps: map[string]bool{}}
	for k, v := range d.Capabilities {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			info.Caps[k] = b
		}
	}
	return info, nil
}

// Gauge reads one scalar series from /metrics (first sample of that name, labelled or not). The second result is false when the endpoint answered
// without the series, so "absent" is never read as zero.
func (l *LlamaClient) Gauge(ctx context.Context, port int, series string) (float64, bool, error) {
	raw, err := l.get(ctx, port, "/metrics")
	if err != nil {
		return 0, false, err
	}
	v, ok := parsePromScalar(string(raw), series)
	return v, ok, nil
}
