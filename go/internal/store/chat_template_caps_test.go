// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// TestUpdateConfigChatTemplateCaps covers 0083_chat_template_caps.sql (T1,
// per-request thinking control): the narrow probe-write path must persist
// the probed map + a fresh probed_at, and must never touch
// ChatTemplateCapsOverride — that column is only ever written by the
// regular Config CRUD path (an operator's curated correction).
func TestUpdateConfigChatTemplateCaps(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	id := seedConfig(t, db, "probed-config")

	// A never-loaded config carries no probed caps and no probed_at.
	c, err := cat.GetConfig(ctx, id)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if len(c.ChatTemplateCaps) != 0 {
		t.Errorf("fresh config ChatTemplateCaps = %+v, want empty", c.ChatTemplateCaps)
	}
	if !c.ChatTemplateCapsProbedAt.IsZero() {
		t.Errorf("fresh config ChatTemplateCapsProbedAt = %v, want zero", c.ChatTemplateCapsProbedAt)
	}
	if c.ChatTemplateCapsOverride != nil {
		t.Errorf("fresh config ChatTemplateCapsOverride = %+v, want nil", c.ChatTemplateCapsOverride)
	}

	probed := map[string]bool{
		"supports_reasoning_effort":   true,
		"supports_preserve_reasoning": true,
		"supports_tool_calls":         true,
		"supports_typed_content":      false,
	}
	if err := cat.UpdateConfigChatTemplateCaps(ctx, id, probed); err != nil {
		t.Fatalf("UpdateConfigChatTemplateCaps: %v", err)
	}

	c, err = cat.GetConfig(ctx, id)
	if err != nil {
		t.Fatalf("GetConfig after probe: %v", err)
	}
	if len(c.ChatTemplateCaps) != len(probed) {
		t.Fatalf("ChatTemplateCaps = %+v, want %+v", c.ChatTemplateCaps, probed)
	}
	for k, v := range probed {
		if c.ChatTemplateCaps[k] != v {
			t.Errorf("ChatTemplateCaps[%q] = %v, want %v", k, c.ChatTemplateCaps[k], v)
		}
	}
	if c.ChatTemplateCapsProbedAt.IsZero() {
		t.Errorf("ChatTemplateCapsProbedAt still zero after a probe write")
	}

	// A curated override must merge OVER the probed value for its key, and
	// leave every other probed key untouched (EffectiveChatTemplateCaps).
	// Set directly via SQL rather than cat.UpdateConfig: seedConfig's
	// placeholder variant_id/weight_artifact_id/engine_id=1 rows don't
	// really exist, and UpdateConfig's full-object write would trip
	// foreign_keys=ON re-affirming them (same reason capability_tier_crud_test.go's
	// seedConfigInCapabilityTier sets its FK column at INSERT time instead of a
	// later UPDATE) — this test only needs to exercise the override's read
	// path (scan + EffectiveChatTemplateCaps merge), which UpdateConfig's own
	// SQL parameter binding for the column doesn't affect.
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE configs SET chat_template_caps_override=? WHERE id=?`,
		`{"supports_tool_calls":false}`, id); err != nil {
		t.Fatalf("set override: %v", err)
	}
	c, _ = cat.GetConfig(ctx, id)
	eff := c.EffectiveChatTemplateCaps()
	if eff["supports_tool_calls"] != false {
		t.Errorf("EffectiveChatTemplateCaps[supports_tool_calls] = %v, want false (override should win)", eff["supports_tool_calls"])
	}
	if eff["supports_reasoning_effort"] != true {
		t.Errorf("EffectiveChatTemplateCaps[supports_reasoning_effort] = %v, want true (untouched probed value)", eff["supports_reasoning_effort"])
	}
	// The raw probed map itself must be unchanged by the override write.
	if c.ChatTemplateCaps["supports_tool_calls"] != true {
		t.Errorf("raw ChatTemplateCaps[supports_tool_calls] = %v, want true (override must not mutate the probed fact)", c.ChatTemplateCaps["supports_tool_calls"])
	}

	// A subsequent fresh probe (e.g. a reload) must not be clobbered by, or
	// clobber, the override.
	if err := cat.UpdateConfigChatTemplateCaps(ctx, id, map[string]bool{"supports_tool_calls": true, "supports_reasoning_effort": false}); err != nil {
		t.Fatalf("UpdateConfigChatTemplateCaps (reprobe): %v", err)
	}
	c, _ = cat.GetConfig(ctx, id)
	if c.ChatTemplateCapsOverride["supports_tool_calls"] != false {
		t.Errorf("override was clobbered by a reprobe: %+v", c.ChatTemplateCapsOverride)
	}
	eff = c.EffectiveChatTemplateCaps()
	if eff["supports_tool_calls"] != false {
		t.Errorf("EffectiveChatTemplateCaps[supports_tool_calls] after reprobe = %v, want false (override still wins)", eff["supports_tool_calls"])
	}
	if eff["supports_reasoning_effort"] != false {
		t.Errorf("EffectiveChatTemplateCaps[supports_reasoning_effort] after reprobe = %v, want false (fresh probed value, no override)", eff["supports_reasoning_effort"])
	}

	if err := cat.UpdateConfigChatTemplateCaps(ctx, 99999, map[string]bool{"x": true}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateConfigChatTemplateCaps missing config: got %v, want ErrNotFound", err)
	}
}
