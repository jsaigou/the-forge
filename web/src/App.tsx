import { lazy, Suspense, useEffect, useRef, useState } from "react";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { MaintenanceBanner } from "./components/MaintenanceBanner";
import { useProfileProgress, useProfileRunTracker, useStatus } from "./lib/queries";
import { useLiveEvents } from "./lib/sse";
import { useSession } from "./lib/session";
import { OnboardingTour } from "./onboarding/OnboardingTour";
import { Attributions } from "./pages/Attributions";
import { Console } from "./pages/Console";
import { Dashboard } from "./pages/Dashboard";

// Mobile F7: route-level code splitting — these four load on first visit
// so the initial JS payload drops well below the old 770 KB single bundle.
// Console/Dashboard stay eager (landing surfaces).
const Models = lazy(() => import("./pages/Models").then((m) => ({ default: m.Models })));
const Scheduling = lazy(() => import("./pages/Scheduling").then((m) => ({ default: m.Scheduling })));
const Settings = lazy(() => import("./pages/Settings").then((m) => ({ default: m.Settings })));
const Help = lazy(() => import("./pages/Help").then((m) => ({ default: m.Help })));

// All routable tabs. Wave 2 nav reorg (docs/v5-smith-wave2.md §3 track
// W2-B) promoted Help and Attributions from footer links to top-nav pills.
// Pre-release feedback round (2026-08-12) partially reverses that for
// Attributions only — it stays routable (isTabKey/parseHash/#attributions
// deep links keep working unchanged) but is excluded from the pill row
// below and rendered as a footer link instead, per operator feedback.
const TABS = [
  { key: "console", label: "Console", Component: Console },
  { key: "dashboard", label: "Dashboard", Component: Dashboard },
  { key: "models", label: "Models", Component: Models },
  { key: "scheduling", label: "Scheduling", Component: Scheduling },
  { key: "settings", label: "Settings", Component: Settings },
  { key: "help", label: "Help", Component: Help },
  { key: "attributions", label: "Attributions", Component: Attributions },
] as const;

const NAV_TABS = TABS.filter((t) => t.key !== "attributions");

type TabKey = (typeof TABS)[number]["key"];

function isTabKey(v: unknown): v is TabKey {
  return typeof v === "string" && TABS.some((t) => t.key === v);
}

// ── Tab routing on real browser history ─────────────────────────────────────
//
// Previously tab state was a bare useState (App.tsx) with zero history
// entries — Back from any tab left the SPA entirely, and forward/back never
// worked at all. Two product requirements drove this:
//   1. On Console (the landing tab), Back should prompt before leaving the
//      app rather than silently navigating away.
//   2. On any other tab, Back should return to the previously open tab.
//
// Each tab switch pushes a real history entry keyed {app:true, tab}, so (2)
// is just normal browser history: popstate fires with that entry's state
// and we switch to it directly, no prompt. (1) only needs to fire when a
// Back press would actually unload the document — i.e. there's no earlier
// in-app entry left to pop to, which is exactly when Console (the landing
// tab, hence the first entry this app ever pushes) is current. That case is
// NOT a popstate at all (popstate only fires for same-document history
// transitions); it's a real navigation-away, so the correct hook is the
// standard `beforeunload` guard, armed only while tab === "console". This
// composes correctly with no extra bookkeeping: if there's an earlier tab
// entry, popstate handles it first and beforeunload never fires; only when
// there truly isn't one does the browser attempt to unload, at which point
// beforeunload's native "leave site?" prompt is exactly the requested
// "prompt before it navigates off the page."
//
// Sprint 12 (was H) Phase 4: extended for a `tab/sub` hash shape so Settings
// can deep-link into a section (e.g. #settings/security). Three real gaps
// fixed along the way, not just additive: (a) the mount effect used to
// unconditionally replaceState to bare `#${tab}`, which is what silently
// erased any sub-route on a fresh load — a hash like #settings/security
// pasted into the address bar landed on Settings but with no sub parsed
// out, then got its own hash overwritten to plain #settings; (b) there was
// no hashchange listener at all (only popstate, which never fires for a
// same-document hash edit) — editing an already-open tab's hash did
// nothing, a known pre-existing quirk this sprint's search feature (Phase
// 8) needs fixed since search results navigate via hash; (c) setTab's
// no-op guard only compared `tab`, so calling it with a new `sub` but the
// same top-level tab silently did nothing.
function parseHash(): { tab: TabKey; sub?: string } {
  const raw = location.hash.slice(1);
  const slash = raw.indexOf("/");
  const tabPart = slash === -1 ? raw : raw.slice(0, slash);
  const subPart = slash === -1 ? undefined : raw.slice(slash + 1) || undefined;
  return isTabKey(tabPart) ? { tab: tabPart, sub: subPart } : { tab: "console" };
}

function hashFor(tab: TabKey, sub?: string): string {
  return sub ? `${tab}/${sub}` : tab;
}

function useTabRouter(): {
  tab: TabKey;
  sub?: string;
  setTab: (t: TabKey, sub?: string) => void;
  setSub: (sub: string, opts?: { replace?: boolean }) => void;
} {
  const initial = parseHash();
  const [tab, setTabState] = useState<TabKey>(initial.tab);
  const [sub, setSubState] = useState<string | undefined>(initial.sub);
  const tabRef = useRef(tab);
  tabRef.current = tab;
  const subRef = useRef(sub);
  subRef.current = sub;

  useEffect(() => {
    // Tag the landing entry so Forward back to it is recognized too —
    // preserving whatever sub-route was in the initial hash rather than
    // collapsing it to the bare tab (see (a) above).
    //
    // Guarded on history.state already being app-tagged: React fires child
    // effects before parent effects within the same commit, so a page's own
    // corrective effect (Settings.tsx redirecting a retired deep-link slug,
    // or canonicalizing an invalid section) runs BEFORE this one and may
    // have already called replaceState with the corrected sub. Blindly
    // re-stamping `initial` here — captured once, before any correction —
    // would silently clobber that fix back to the stale hash. A real bug
    // caught live: #settings/catalog/profiling rendered the (correct)
    // redirected Profiling section but the address bar stayed on the
    // retired slug forever, because this effect kept winning the race.
    if (!(history.state as { app?: boolean } | null)?.app) {
      history.replaceState({ app: true, tab: initial.tab, sub: initial.sub }, "", `#${hashFor(initial.tab, initial.sub)}`);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    function onPopState(e: PopStateEvent) {
      const state = e.state as { app?: boolean; tab?: TabKey; sub?: string } | null;
      if (state?.app && state.tab) {
        setTabState(state.tab);
        setSubState(state.sub);
      }
    }
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  useEffect(() => {
    // See (b) above — this is new. Guarded against reacting to a hash that
    // already matches current state, since our own pushState/replaceState
    // calls below don't fire hashchange themselves, but a listener that
    // didn't guard could still double-process a change it initiated.
    function onHashChange() {
      const parsed = parseHash();
      if (parsed.tab === tabRef.current && parsed.sub === subRef.current) return;
      setTabState(parsed.tab);
      setSubState(parsed.sub);
    }
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    if (tab !== "console") return;
    function onBeforeUnload(e: BeforeUnloadEvent) {
      e.preventDefault();
      e.returnValue = "";
    }
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [tab]);

  function setTab(t: TabKey, newSub?: string) {
    if (t === tabRef.current && newSub === subRef.current) return; // see (c) above
    history.pushState({ app: true, tab: t, sub: newSub }, "", `#${hashFor(t, newSub)}`);
    setTabState(t);
    setSubState(newSub);
  }

  // setSub changes only the sub-route within the current tab. Sidebar
  // clicks push (so Back returns to the previously open section — the same
  // per-tab-switch history rationale documented above, just one level
  // deeper); anchor scrolls / canonicalizing an invalid section replace (no
  // new history entry for a correction the user didn't ask to navigate to).
  function setSub(newSub: string, opts?: { replace?: boolean }) {
    const t = tabRef.current;
    if (newSub === subRef.current) return;
    const url = `#${hashFor(t, newSub)}`;
    if (opts?.replace) {
      history.replaceState({ app: true, tab: t, sub: newSub }, "", url);
    } else {
      history.pushState({ app: true, tab: t, sub: newSub }, "", url);
    }
    setSubState(newSub);
  }

  return { tab, sub, setTab, setSub };
}

const THEME_KEY = "foundry.console.theme";

function useThemeToggle() {
  const [theme, setTheme] = useState<"light" | "dark" | null>(() => {
    const saved = localStorage.getItem(THEME_KEY);
    return saved === "light" || saved === "dark" ? saved : null;
  });

  useEffect(() => {
    if (theme) {
      document.documentElement.dataset.theme = theme;
      localStorage.setItem(THEME_KEY, theme);
    } else {
      delete document.documentElement.dataset.theme;
      localStorage.removeItem(THEME_KEY);
    }
  }, [theme]);

  const toggle = () => {
    setTheme((cur) => {
      if (cur === "dark") return "light";
      if (cur === "light") return "dark";
      return matchMedia("(prefers-color-scheme: dark)").matches ? "light" : "dark";
    });
  };

  return toggle;
}

export function App() {
  const { tab, sub, setTab, setSub } = useTabRouter();
  const { username, role } = useSession();
  const toggleTheme = useThemeToggle();
  useLiveEvents();
  useProfileRunTracker(); // single global poll+finalize driver — see queries.ts
  const profileProgress = useProfileProgress();
  const status = useStatus();
  const restartRequired = status.data?.restart_required;

  const Active = TABS.find((t) => t.key === tab)!.Component;

  return (
    <div className="wrap">
      <div className="top">
        <div className="brand">
          <img src="/favicon.svg" alt="" className="brand-mark" width={34} height={34} />
          <div>
            <h1>The Forge</h1>
          </div>
        </div>
        <div className="spacer" />
        <MaintenanceBanner />
        {/* Restart-required pip (Sprint 12 Phase 6) — visible from any tab,
            same "small dot, click to jump to the detail" idiom as the
            profile throbber below. The fuller banner lives on Settings →
            General (settings/panels/General.tsx's RestartBanner). */}
        {restartRequired && (
          <button
            className="icon-btn"
            title={`Restart required — ${restartRequired.keys.length} setting(s) changed since boot, by ${restartRequired.by}. Click for detail.`}
            onClick={() => setTab("settings", "general")}
            style={{ display: "flex", alignItems: "center", justifyContent: "center", padding: 0, marginRight: 6 }}
          >
            <span
              style={{
                width: 8, height: 8, borderRadius: "50%",
                background: "var(--warn)", boxShadow: "0 0 7px var(--warn)",
              }}
            />
          </button>
        )}
        {/* Profile run throbber — visible from any tab. Phase 8 (pre-release
            feedback sprint): now a button matching the restart-pip idiom
            above (click jumps to the detail) — before this it was a
            non-interactive div with nowhere to send you; now that
            profiling lives inside Settings → Benchmarks & Profiling, it
            has somewhere to point. */}
        {profileProgress?.running && (
          <button
            className="icon-btn"
            title={`Profiling ${profileProgress.mode ?? ""} — ${profileProgress.phase}. Click for detail.`}
            onClick={() => setTab("settings", "benchmarks")}
            style={{ display: "flex", alignItems: "center", justifyContent: "center", padding: 0, marginRight: 10 }}
          >
            <span
              className="dot-busy"
              style={{
                width: 8, height: 8, borderRadius: "50%",
                background: "var(--warn, var(--accent))",
                boxShadow: "0 0 7px var(--warn, var(--accent))",
              }}
            />
          </button>
        )}
        <nav className="tabs">
          {NAV_TABS.map((t) => (
            <button key={t.key} className={`tab ${tab === t.key ? "active" : ""}`} onClick={() => setTab(t.key)}>
              {t.label}
            </button>
          ))}
        </nav>
        <span className="username" title={`role: ${role}`}>{username}</span>
        <button className="icon-btn" title="Toggle theme" onClick={toggleTheme}>◐</button>
      </div>
      <ErrorBoundary>
        <Suspense fallback={<div className="empty-note">Loading…</div>}>
          {tab === "settings" ? <Settings sub={sub} onSubChange={setSub} /> : tab === "help" ? <Help sub={sub} onSubChange={setSub} /> : <Active />}
        </Suspense>
      </ErrorBoundary>
      {/* First-run guided tour (P4) — self-gating via useOnboarding: renders
          nothing once dismissed/replayed-flagged, so dismissed browsers pay
          zero cost beyond one localStorage read. Portaled to body anyway. */}
      <OnboardingTour />
      <div className="foot">
        <span><b>{role}</b> · {username}</span>
        <a href="#attributions" className={tab === "attributions" ? "active" : undefined}>Attributions</a>
      </div>
    </div>
  );
}
