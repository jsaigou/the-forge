// SPDX-License-Identifier: Apache-2.0

package ctxledger

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

func promEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// WriteMetrics renders the C2 counters in Prometheus text format (counters
// since process start; tool label limited to the top 50 by chars + "other").
func (l *Ledger) WriteMetrics(w io.Writer) {
	if l == nil {
		return
	}
	l.mu.Lock()
	created := map[string]map[string]int64{}
	for c, m := range l.created {
		cp := map[string]int64{}
		for k, v := range m {
			cp[k] = v
		}
		created[c] = cp
	}
	toolOut := map[string]int64{}
	for k, v := range l.toolOut {
		toolOut[k] = v
	}
	suspect := map[string]int64{}
	for k, v := range l.suspect {
		suspect[k] = v
	}
	l.mu.Unlock()

	_, _ = fmt.Fprintln(w, "# TYPE context_created_chars_total counter")
	for _, c := range sortedKeys(created) {
		kinds := make([]string, 0, len(created[c]))
		for k := range created[c] {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			_, _ = fmt.Fprintf(w, "context_created_chars_total{consumer=\"%s\",kind=\"%s\"} %d\n", promEscape(c), k, created[c][k])
		}
	}
	_, _ = fmt.Fprintln(w, "# TYPE context_tool_output_chars_total counter")
	names := make([]string, 0, len(toolOut))
	for k := range toolOut {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if toolOut[names[i]] != toolOut[names[j]] {
			return toolOut[names[i]] > toolOut[names[j]]
		}
		return names[i] < names[j]
	})
	var other int64
	for i, n := range names {
		if i < metricsTopTools {
			_, _ = fmt.Fprintf(w, "context_tool_output_chars_total{tool=\"%s\"} %d\n", promEscape(n), toolOut[n])
		} else {
			other += toolOut[n]
		}
	}
	if other > 0 {
		_, _ = fmt.Fprintf(w, "context_tool_output_chars_total{tool=\"other\"} %d\n", other)
	}
	_, _ = fmt.Fprintln(w, "# TYPE context_cache_bust_suspect_total counter")
	for _, c := range sortedKeys(suspect) {
		_, _ = fmt.Fprintf(w, "context_cache_bust_suspect_total{consumer=\"%s\"} %d\n", promEscape(c), suspect[c])
	}
	st := l.Stats()
	_, _ = fmt.Fprintln(w, "# TYPE context_ledger_observed_total counter")
	_, _ = fmt.Fprintf(w, "context_ledger_observed_total %d\ncontext_ledger_dropped_total %d\ncontext_ledger_errors_total %d\n",
		st.Observed, st.Dropped, st.ParseFailures+st.Panics+st.FlushErrors)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
