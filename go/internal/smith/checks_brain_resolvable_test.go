// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
)

func TestBrainResolvableSeverity(t *testing.T) {
	ctx := context.Background()

	t.Run("ok when resolved to a local slot", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"ornith-35b"`)
		s := New(Deps{
			Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
			Sched: newStubSched(map[string]string{"a3": "ornith-35b"}),
		})
		f := runOne(ctx, findCheck(t, "brain_resolvable"), s.checkEnv(ctx))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})

	t.Run("ok when resolved to a remote offering", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"deepseek-chat"`)
		s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog()})
		f := runOne(ctx, findCheck(t, "brain_resolvable"), s.checkEnv(ctx))
		if f.Severity != SeverityOK {
			t.Errorf("severity = %s, want ok (summary %q)", f.Severity, f.Summary)
		}
	})

	t.Run("warn when smith.model unset", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog()})
		f := runOne(ctx, findCheck(t, "brain_resolvable"), s.checkEnv(ctx))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})

	t.Run("warn when configured model is not loaded on any slot", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"ornith-35b"`)
		s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: newStubSched(map[string]string{})})
		f := runOne(ctx, findCheck(t, "brain_resolvable"), s.checkEnv(ctx))
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %s, want warn (summary %q)", f.Severity, f.Summary)
		}
	})

	t.Run("skip when store unwired", func(t *testing.T) {
		s := New(Deps{})
		f := runOne(ctx, findCheck(t, "brain_resolvable"), s.checkEnv(ctx))
		if f.Severity != SeverityInfo {
			t.Errorf("severity = %s, want info (skipped)", f.Severity)
		}
	})
}
