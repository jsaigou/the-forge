import { useEffect, useRef, type ReactElement } from "react";
import { CatalogPanel, type SubTab as CatalogSubTab } from "../components/CatalogPanel";
import { ErrorBoundary } from "../components/ErrorBoundary";
import { SecurityPanel } from "../components/SecurityPanel";
import { useSession } from "../lib/session";
import { Benchmarks } from "../settings/panels/Benchmarks";
import { Billing } from "../settings/panels/Billing";
import { Danger } from "../settings/panels/Danger";
import { General } from "../settings/panels/General";
import { Monitoring } from "../settings/panels/Monitoring";
import { ProviderKeys } from "../settings/panels/ProviderKeys";
import { Routing } from "../settings/panels/Routing";
import { SchedulingSettings } from "../settings/panels/Scheduling";
import { Smith } from "../settings/panels/Smith";
import { Voice } from "../settings/panels/Voice";
import { DEFAULT_SECTION, SETTINGS_GROUPS, SETTINGS_SECTIONS, isSectionKey, type SectionKey } from "../settings/sections";
import { SettingsSearch } from "../settings/SettingsSearch";

// FE-4 Settings page (Sprint 0 §0.9, folded per docs/v5-review-fixes.md FE-4).
// Sprint 12 (was H): rebuilt around a left-sidebar shell
// (settings/sections.tsx has the section→group metadata) so only one
// section's panel mounts at a time, instead of all six stacked and mounted
// simultaneously as before Phase 4.
//
// Phase 5: every panel now lives in its own file under settings/panels/ (or,
// for Benchmarks/Profiling, is promoted straight from where it already
// lived — components/CatalogPanel.tsx's BenchmarksSection and the
// already-self-contained components/ProfilingPanel.tsx). Currency and
// Cost & power merged into one "Billing" section (two cards). This file is
// now just the shell: section metadata → component wiring, deep-link
// parsing/canonicalization, and the retired-slug redirect for the two
// catalog sub-tabs that moved out (#settings/catalog/benchmarks and
// #settings/catalog/profiling both predate this phase and may be
// bookmarked).
//
// Phase 8 (pre-release feedback sprint, 2026-08-13): "profiling" was folded
// into "benchmarks" — the destructive profiling run action now lives
// per-config inside the merged Benchmarks & Profiling view
// (settings/panels/Benchmarks.tsx), since profiling PRODUCES exactly the
// kind of data (measured memory, prefill/decode T/s) that belongs next to
// the curated benchmarks it should be compared against. ProfilingPanel.tsx
// itself is deleted. Same redirect shape as the earlier compressor→routing
// merge below.
//
// Mutating routes are requireRole(admin) on the backend (settings_handlers.go);
// non-admin viewers see a read-only render and the forms are hidden.

// Retired catalog sub-tab slugs → their new top-level section. A stale
// bookmark/link to #settings/catalog/benchmarks (or /profiling) must still
// land somewhere sensible rather than silently 404ing into whatever
// CatalogPanel's default tab happens to be.
const RETIRED_CATALOG_SLUGS: Record<string, SectionKey> = {
  "catalog/benchmarks": "benchmarks",
  "catalog/profiling": "benchmarks",
};

// Retired top-level sections → their replacement. "compressor" was folded
// into "routing" Phase 7 (2026-08-13, Routing and Compressor are two halves
// of one request path — see Routing.tsx's header comment); "profiling" was
// folded into "benchmarks" Phase 8, same day (see the header comment
// above). This maps just the section key — any anchor
// (#settings/compressor/compressor-mode, #settings/profiling/profiling-runs)
// is preserved and reattached to the new section, since both merged panels
// still render the same DOM ids their old standalone pages did.
const RETIRED_SECTION_SLUGS: Record<string, SectionKey> = {
  compressor: "routing",
  profiling: "benchmarks",
};

function useSectionComponents(catalogAnchor: string | undefined, onCatalogTabChange: (tab: CatalogSubTab) => void): Record<SectionKey, () => ReactElement> {
  const { canAdmin, canOperate } = useSession();
  return {
    general: () => <General canAdmin={canAdmin} />,
    providers: () => <ProviderKeys canAdmin={canAdmin} />,
    billing: () => <Billing canAdmin={canAdmin} />,
    routing: () => <Routing canAdmin={canAdmin} canOperate={canOperate} />,
    scheduling: () => <SchedulingSettings canAdmin={canAdmin} />,
    monitoring: () => <Monitoring canAdmin={canAdmin} />,
    smith: () => <Smith canAdmin={canAdmin} />,
    voice: () => <Voice canAdmin={canAdmin} />,
    catalog: () => <CatalogPanel canAdmin={canAdmin} tab={isCatalogSubTab(catalogAnchor) ? catalogAnchor : undefined} onTabChange={onCatalogTabChange} />,
    benchmarks: () => <Benchmarks canAdmin={canAdmin} />,
    security: () => <SecurityPanel canAdmin={canAdmin} />,
    danger: () => <Danger canAdmin={canAdmin} />,
  };
}

const CATALOG_SUB_TABS: CatalogSubTab[] = ["configs", "offerings", "models", "taxonomy", "notes", "services"];
function isCatalogSubTab(v: string | undefined): v is CatalogSubTab {
  return !!v && (CATALOG_SUB_TABS as string[]).includes(v);
}

export function Settings({ sub, onSubChange }: { sub?: string; onSubChange?: (sub: string, opts?: { replace?: boolean }) => void }) {
  const [sectionPart, anchorPart] = (sub ?? "").split("/");
  const retiredCatalog = sub ? RETIRED_CATALOG_SLUGS[sub] : undefined;
  const retiredSection = RETIRED_SECTION_SLUGS[sectionPart];
  const activeKey: SectionKey = retiredCatalog ?? retiredSection ?? (isSectionKey(sectionPart) ? sectionPart : DEFAULT_SECTION);
  const flashTimer = useRef<number | undefined>(undefined);

  const sectionComponents = useSectionComponents(
    activeKey === "catalog" ? anchorPart : undefined,
    (tab) => onSubChange?.(`catalog/${tab}`, { replace: true }),
  );

  useEffect(() => {
    if (retiredCatalog) {
      // A bookmark to a sub-tab that no longer exists inside Catalog —
      // land on its new top-level section instead of a blank/default pane.
      onSubChange?.(retiredCatalog, { replace: true });
    } else if (retiredSection) {
      // A bookmark to a section that was folded into another one — keep
      // any anchor (e.g. #settings/compressor/compressor-mode ->
      // #settings/routing/compressor-mode), the merged panel still renders
      // the same DOM ids.
      onSubChange?.(anchorPart ? `${retiredSection}/${anchorPart}` : retiredSection, { replace: true });
    } else if (!isSectionKey(sectionPart)) {
      // Canonicalize an absent or unrecognized section (a bare #settings,
      // a stale/typo'd hash) to the default — a replace, not a push, since
      // this is a correction the operator didn't ask to navigate to.
      onSubChange?.(DEFAULT_SECTION, { replace: true });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sub]);

  // Sprint 13 (Sprint 12 Phase 8): anchor navigation — search results and
  // deep links like #settings/security/security-api-keys scroll to the
  // anchor and flash it (.anchor-flash, defined + reduced-motion-gated in
  // theme.css). rAF so a cross-section jump scrolls AFTER the new panel's
  // first paint. Catalog sub-tab anchors are tab slugs with no DOM id —
  // getElementById finds nothing and this no-ops (the tab switch itself is
  // the navigation there).
  useEffect(() => {
    if (!anchorPart) return;
    const raf = requestAnimationFrame(() => {
      const el = document.getElementById(anchorPart);
      if (!el) return;
      const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      el.scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "start" });
      // remove→reflow→add restarts the flash on repeat navigation to the
      // same anchor.
      el.classList.remove("anchor-flash");
      void el.offsetWidth;
      el.classList.add("anchor-flash");
      window.clearTimeout(flashTimer.current);
      flashTimer.current = window.setTimeout(() => el.classList.remove("anchor-flash"), 1700);
    });
    return () => {
      cancelAnimationFrame(raf);
      window.clearTimeout(flashTimer.current);
    };
  }, [sub, anchorPart]);

  const ActiveComponent = sectionComponents[activeKey];

  return (
    <section className="page settings-shell">
      <nav className="side-nav" aria-label="Settings sections">
        {SETTINGS_GROUPS.map((group) => (
          <div key={group}>
            <div className="side-nav-group">{group}</div>
            {SETTINGS_SECTIONS.filter((s) => s.group === group).map((s) => (
              <button
                key={s.key}
                className={`side-nav-item ${s.danger ? "danger" : ""} ${activeKey === s.key ? "active" : ""}`}
                onClick={() => onSubChange?.(s.key)}
              >
                {s.label}
              </button>
            ))}
          </div>
        ))}
      </nav>
      {/* Mobile fallback (≤860px, .settings-shell hides .side-nav there —
          see theme.css) — same sections, flat pill row instead of grouped. */}
      <div className="tabs">
        {SETTINGS_SECTIONS.map((s) => (
          <button
            key={s.key}
            className={`tab ${activeKey === s.key ? "active" : ""}`}
            onClick={() => onSubChange?.(s.key)}
          >
            {s.label}
          </button>
        ))}
      </div>
      <div>
        {/* Sprint 13 (Sprint 12 Phase 8) — search over the whole settings
            registry (fields + landmarks). Cross-section jumps push (Back
            returns to the section you left, same rationale as sidebar
            clicks); same-section anchor scrolls replace. */}
        <SettingsSearch
          onNavigate={(section, anchor) => {
            const target = anchor ? `${section}/${anchor}` : section;
            onSubChange?.(target, { replace: section === activeKey });
          }}
        />
        <ErrorBoundary resetKeys={[activeKey]}>
          <ActiveComponent />
        </ErrorBoundary>
      </div>
    </section>
  );
}
