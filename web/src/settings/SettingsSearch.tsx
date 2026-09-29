import { useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { fieldLabel } from "./Field";
import { SETTINGS_INDEX, type SettingRecord } from "./fields";
import { SETTINGS_SECTIONS, type SectionKey } from "./sections";
import { SAVE_FLASH_MS, msSinceLastSaveSuccess } from "./useSettingsGroup";

// settings/SettingsSearch.tsx — Sprint 13 (Sprint 12 Phase 8). The first
// search input in the codebase, deliberately minimal per the plan's Search
// section: lowercased `.includes` over label+id+storeKey+keywords+help
// (+ the section label, so searching "security" finds the security
// landmarks), no fuzzy library, no useDeferredValue — ~90 records makes a
// memoized filter instant. `/` focuses from anywhere on the Settings page,
// suppressed inside any input and while any modal is open so it never
// fights StepUpModal/DetailModal. Results navigate section/anchor: fields
// land on their own DOM id, landmarks on their recorded anchor (a DOM id or
// a Catalog sub-tab slug).
//
// The dropdown stays open through a result click via onMouseDown
// preventDefault (a plain blur-on-mousedown would hide the row before the
// click lands); pick() then clears and blurs explicitly.

const MAX_RESULTS = 20;

// English-only, used for matching ONLY (never displayed) — a Japanese
// operator must still be able to find a setting by its English name, and
// SETTINGS_INDEX/SETTINGS_SECTIONS themselves stay literal English forever
// (see fields.ts's own i18n comment). Display uses fieldLabel()/t("sections.*")
// instead — see the render below.
const SECTION_LABEL = Object.fromEntries(SETTINGS_SECTIONS.map((s) => [s.key, s.label])) as Record<SectionKey, string>;

// Haystacks precomputed once at module scope — the index is static.
const INDEX = SETTINGS_INDEX.map((rec) => ({
  rec,
  hay: [rec.label, rec.id, rec.storeKey, ...(rec.keywords ?? []), rec.help, SECTION_LABEL[rec.section]]
    .join(" ")
    .toLowerCase(),
}));

export function SettingsSearch({
  onNavigate,
}: {
  onNavigate: (section: SectionKey, anchor: string | undefined) => void;
}) {
  const { t } = useTranslation("settings");
  const [query, setQuery] = useState("");
  const [focused, setFocused] = useState(false);
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return [];
    return INDEX.filter((e) => e.hay.includes(q)).map((e) => e.rec);
  }, [query]);
  const results = matches.slice(0, MAX_RESULTS);

  useEffect(() => setCursor(0), [query]);

  // `/` focuses the box. The listener lives here (not app-globally) so it
  // only exists while the Settings page is mounted.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)) return;
      if (document.querySelector(".modal-backdrop")) return;
      e.preventDefault();
      inputRef.current?.focus();
      inputRef.current?.select();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  function pick(rec: SettingRecord) {
    const anchor = rec.kind === "field" ? rec.id : rec.anchor;
    const go = () => {
      onNavigate(rec.section, anchor);
      setQuery("");
      setCursor(0);
      inputRef.current?.blur();
    };
    // Plan risk 1: never unmount a panel mid-✓-flash — defer the jump by
    // whatever is left of the 700ms window after the most recent save.
    const wait = SAVE_FLASH_MS - msSinceLastSaveSuccess();
    if (wait > 0) window.setTimeout(go, wait);
    else go();
  }

  function onKeyDown(e: ReactKeyboardEvent<HTMLInputElement>) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor((c) => Math.min(c + 1, results.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor((c) => Math.max(c - 1, 0));
    } else if (e.key === "Enter") {
      const rec = results[cursor] ?? results[0];
      if (rec) pick(rec);
    } else if (e.key === "Escape") {
      setQuery("");
      inputRef.current?.blur();
    }
  }

  const open = focused && query.trim().length > 0;

  return (
    <div className="settings-search">
      <input
        ref={inputRef}
        type="search"
        placeholder={t("search.placeholder")}
        aria-label={t("search.aria_label")}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onFocus={() => setFocused(true)}
        onBlur={() => setFocused(false)}
        onKeyDown={onKeyDown}
      />
      {open && (
        // mousedown-preventDefault keeps focus in the input so the dropdown
        // doesn't vanish before the click on a result registers.
        <div className="search-results" onMouseDown={(e) => e.preventDefault()}>
          {results.map((rec, i) => (
            <div
              key={`${rec.section}:${rec.id}`}
              className={`search-result ${i === cursor ? "active" : ""}`}
              onClick={() => pick(rec)}
            >
              <span>{fieldLabel(t, rec)}</span>
              <span className="path">
                {rec.kind === "field" ? `${t(`sections.${rec.section}`)} · ${rec.storeKey}` : t(`sections.${rec.section}`)}
              </span>
            </div>
          ))}
          {matches.length > MAX_RESULTS && (
            <div className="search-result" style={{ cursor: "default", color: "var(--text-mute)" }}>
              {t("search.more_results", { count: matches.length - MAX_RESULTS })}
            </div>
          )}
          {results.length === 0 && (
            <div className="search-result" style={{ cursor: "default", color: "var(--text-mute)" }}>
              {t("search.no_matches", { query: query.trim() })}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
