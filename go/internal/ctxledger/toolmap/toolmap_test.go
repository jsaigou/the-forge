package toolmap

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Real names, observed 2026-10-04 (names only, no content):
// OpenCode: tool names in the local opencode.db `part` rows (type=tool);
// LibreChat: MCP tool lists logged by the librechat container on core
// (servers obsidian, obsidian-external, docling, webfetch) plus its
// "<tool>_mcp_<server>" wire convention.
var table = []struct{ raw, want string }{
	// OpenCode built-ins
	{"bash", "bash"}, {"read", "read"}, {"edit", "edit"}, {"grep", "grep"},
	{"write", "write"}, {"webfetch", "webfetch"}, {"todowrite", "todowrite"},
	{"glob", "glob"}, {"task", "task"}, {"question", "question"},
	{"skill", "skill"}, {"websearch", "websearch"}, {"invalid", "invalid"},
	// OpenCode MCP (server_tool, kept whole)
	{"deja_recall", "deja_recall"}, {"deja_remember", "deja_remember"},
	{"deja_recall_context", "deja_recall_context"}, {"deja_how", "deja_how"},
	{"deja_fix", "deja_fix"}, {"deja_blame", "deja_blame"},
	{"list_mcp_resources", "list_mcp_resources"},
	{"list_mcp_resource_templates", "list_mcp_resource_templates"},
	{"read_mcp_resource", "read_mcp_resource"},
	// LibreChat webfetch server
	{"web_fetch", "web_fetch"}, {"web_fetch_mcp_webfetch", "web_fetch"},
	// LibreChat obsidian / obsidian-external (mcpvault)
	{"read_note_mcp_obsidian", "read_note"},
	{"write_note_mcp_obsidian-external", "write_note"},
	{"patch_note_mcp_obsidian", "patch_note"},
	{"search_notes_mcp_obsidian", "search_notes"},
	{"list_directory_mcp_obsidian-external", "list_directory"},
	{"read_multiple_notes_mcp_obsidian", "read_multiple_notes"},
	{"get_frontmatter_mcp_obsidian", "get_frontmatter"},
	{"update_frontmatter_mcp_obsidian", "update_frontmatter"},
	{"manage_tags_mcp_obsidian", "manage_tags"},
	{"get_vault_stats_mcp_obsidian-external", "get_vault_stats"},
	{"list_all_tags_mcp_obsidian", "list_all_tags"},
	{"delete_note", "delete_note"}, {"move_note", "move_note"},
	// LibreChat docling
	{"convert_document_into_docling_document_mcp_docling", "convert_document_into_docling_document"},
	{"convert_directory_files_into_docling_document_mcp_docling", "convert_directory_files_into_docling_document"},
	{"get_overview_of_document_anchors_mcp_docling", "get_overview_of_document_anchors"},
	{"search_for_text_in_document_anchors_mcp_docling", "search_for_text_in_document_anchors"},
	{"get_text_of_document_item_at_anchor_mcp_docling", "get_text_of_document_item_at_anchor"},
	{"update_text_of_document_item_at_anchor_mcp_docling", "update_text_of_document_item_at_anchor"},
	{"delete_document_items_at_anchors_mcp_docling", "delete_document_items_at_anchors"},
	{"is_document_in_local_cache_mcp_docling", "is_document_in_local_cache"},
	{"drop_document_from_local_cache_mcp_docling", "drop_document_from_local_cache"},
	// other conventions / edge
	{"mcp__obsidian__read_note", "read_note"}, {"functions.bash", "bash"},
	{"Bash", "bash"}, {"  Read ", "read"}, {"web fetch", "web_fetch"},
	{"", Unknown}, {"___", Unknown}, {"mcp__", "mcp"},
}

func TestNormalizeTable(t *testing.T) {
	if len(table) < 25 {
		t.Fatalf("need >=25 real names, have %d", len(table))
	}
	for _, c := range table {
		got := Normalize(c.raw)
		if got != c.want {
			t.Errorf("Normalize(%q)=%q want %q", c.raw, got, c.want)
		}
		if Normalize(got) != got {
			t.Errorf("not idempotent for %q: %q -> %q", c.raw, got, Normalize(got))
		}
	}
}

func TestNormalizeBounds(t *testing.T) {
	long := strings.Repeat("a", 100000)
	if g := Normalize(long); len(g) != maxNameLen || Normalize(g) != g {
		t.Fatalf("len=%d", len(g))
	}
	if g := Normalize("日本語\x00\xff"); g == "" || Normalize(g) != g {
		t.Fatal(g)
	}
}

func msgs(t *testing.T, s string) []any {
	t.Helper()
	var m []any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestResolve(t *testing.T) {
	m := msgs(t, `[
	 {"role":"user","content":"x"},
	 {"role":"assistant","content":null,"tool_calls":[
	   {"id":"a","type":"function","function":{"name":"read","arguments":"{}"}},
	   {"id":"b","type":"function","function":{"name":"grep","arguments":"{}"}}]},
	 {"role":"tool","tool_call_id":"b","content":null},
	 {"role":"tool","tool_call_id":"a","content":"r"},
	 {"role":"assistant","tool_calls":[{"id":"a","function":{"name":"bash"}}]},
	 {"role":"tool","tool_call_id":"a","content":"r"},
	 {"role":"tool","tool_call_id":"zzz","content":"r"},
	 {"role":"tool","tool_call_id":7,"content":"r"},
	 {"role":"tool","content":"r"},
	 {"role":"function","name":"legacy","content":"r"},
	 {"role":"tool","tool_call_id":"early"},
	 {"role":"assistant","tool_calls":[{"id":"early","function":{"name":"x"}}]}
	]`)
	want := []struct {
		i    int
		name string
		ok   bool
	}{
		{2, "grep", true}, {3, "read", true}, {5, "bash", true}, {6, "", false},
		{7, "", false}, {8, "", false}, {9, "legacy", true}, {10, "", false},
		{0, "", false}, {1, "", false}, {-1, "", false}, {99, "", false},
	}
	for _, w := range want {
		n, ok := Resolve(m, w.i)
		if n != w.name || ok != w.ok {
			t.Errorf("Resolve(%d)=(%q,%v) want (%q,%v)", w.i, n, ok, w.name, w.ok)
		}
	}
	if _, ok := Resolve(nil, 0); ok {
		t.Error("nil")
	}
}

func TestResolveHugeBounded(t *testing.T) {
	m := make([]any, 0, 200003)
	for i := 0; i < 100000; i++ {
		m = append(m, map[string]any{"role": "user", "content": "x"})
	}
	m = append(m, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "q", "function": map[string]any{"name": "read"}}}})
	for i := 0; i < 100000; i++ {
		m = append(m, map[string]any{"role": "tool", "tool_call_id": fmt.Sprint(i)})
	}
	m = append(m, map[string]any{"role": "tool", "tool_call_id": "q"})
	if n, ok := Resolve(m, len(m)-1); !ok || n != "read" {
		t.Fatal(n, ok)
	}
}

func FuzzNormalize(f *testing.F) {
	for _, c := range table {
		f.Add(c.raw)
	}
	f.Add("mcp__a__b__mcp__c_mcp_d_mcp_")
	f.Fuzz(func(t *testing.T, s string) {
		g := Normalize(s)
		if g == "" || len(g) > maxNameLen || Normalize(g) != g {
			t.Fatalf("%q -> %q", s, g)
		}
	})
}

func FuzzResolve(f *testing.F) {
	f.Add(`[{"role":"assistant","tool_calls":[{"id":"a","function":{"name":"r"}}]},{"role":"tool","tool_call_id":"a"}]`, 1)
	f.Add(`[null,1,"x",{"role":7},{"role":"tool","tool_call_id":[]},{"role":"assistant","tool_calls":[null,1,{"function":null}]}]`, 4)
	f.Fuzz(func(t *testing.T, s string, i int) {
		var m []any
		if json.Unmarshal([]byte(s), &m) != nil {
			var x any
			if json.Unmarshal([]byte(s), &x) == nil {
				m = []any{x}
			}
		}
		Resolve(m, i)
	})
}
