// settings/panels/Smith.tsx — P3 (docs/v5-smith.md §6). Four cards:
// "Reasoning" (smith.model + smith.handoff_offerings — both bespoke pickers
// over live catalog data, hence kind:"landmark" in fields.ts rather than
// generic <Field> options), "Schedule" (smith.schedule, plain text fields —
// same shape as Monitoring's number fields but Go durations are strings),
// "Thresholds" (smith.thresholds, four percentage fields, verbatim copy of
// Monitoring's gtt_warn_pct pattern), and "Web research" (P5,
// docs/v5-smith.md §4.8 — smith.web.*: enabled toggle, provider order,
// per-provider base_url/api_key, cache TTL, live reachability chips off
// GET /smith/status, and a manual re-probe button). All four share the same
// GET/PUT /api/v1/smith/settings endpoint (five independent settings-key
// groups under one response shape) — each card runs its own
// useSettingsGroup instance against it, same as Monitoring's Monitor/
// Metrics cards, so an unsaved draft in one card never leaks into another
// card's save.
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../../lib/api";
import { appLocale } from "../../lib/format";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { useCatalogConfigs, useCatalogOfferings, useSmithAutonomy, useSmithSettings, useSmithStatus, useSmithWebProbe, useUpdateSmithAutonomy, useUpdateSmithSettings } from "../../lib/queries";
import type { SmithAutonomyPolicy, SmithAutonomyProcedurePolicy, SmithWebProviderSettings, SmithWebSettings } from "../../lib/types";
import { Field, fieldHelp, fieldLabel } from "../Field";
import { SMITH_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";
import { useStepUpGate } from "../../lib/useStepUpGate";

const F = Object.fromEntries(SMITH_FIELDS.map((f) => [f.id, f]));

function ReasoningCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const configs = useCatalogConfigs();
  const offerings = useCatalogOfferings();

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.reasoning")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: "smith" })}</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.reasoning")}</div>
        <div className="card"><div className="empty-note">{t("shared.loading", { resource: "smith" })}</div></div>
      </>
    );
  }
  const active = g.active;

  const visibleConfigs = (configs.data ?? []).filter((c) => c.visibility !== "hidden");
  const enabledOfferings = (offerings.data ?? []).filter((o) => o.enabled);

  function toggleOffering(id: number) {
    const next = active.handoff_offerings.includes(id)
      ? active.handoff_offerings.filter((x) => x !== id)
      : [...active.handoff_offerings, id];
    g.setField("handoff_offerings", next);
  }
  function moveOffering(id: number, dir: -1 | 1) {
    const arr = [...active.handoff_offerings];
    const i = arr.indexOf(id);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= arr.length) return;
    [arr[i], arr[j]] = [arr[j], arr[i]];
    g.setField("handoff_offerings", arr);
  }
  function toggleBrainChainMember(name: string) {
    const next = active.brain_chain.includes(name)
      ? active.brain_chain.filter((x) => x !== name)
      : [...active.brain_chain, name];
    g.setField("brain_chain", next);
  }
  function moveBrainChainMember(name: string, dir: -1 | 1) {
    const arr = [...active.brain_chain];
    const i = arr.indexOf(name);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= arr.length) return;
    [arr[i], arr[j]] = [arr[j], arr[i]];
    g.setField("brain_chain", arr);
  }

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.reasoning")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}

        <label id="smith-model" className="form-row">
          <span className="sm-row">{fieldLabel(t, F["smith.model"])}</span>
          <select value={active.model} disabled={!canAdmin} onChange={(e) => g.setField("model", e.target.value)}>
            <option value="">{t("smith.reasoning.none_model")}</option>
            {visibleConfigs.length > 0 && (
              <optgroup label={t("smith.reasoning.local_configs")}>
                {visibleConfigs.map((c) => (
                  <option key={`c${c.id}`} value={c.name}>{c.name}</option>
                ))}
              </optgroup>
            )}
            {enabledOfferings.length > 0 && (
              <optgroup label={t("smith.reasoning.remote_offerings")}>
                {enabledOfferings.map((o) => (
                  <option key={`o${o.id}`} value={o.wire_model}>
                    {o.wire_model} ({o.provider})
                  </option>
                ))}
              </optgroup>
            )}
          </select>
        </label>
        <div className="sm-note-lead">
          {fieldHelp(t, F["smith.model"])}
        </div>

        <div id="smith-handoff-offerings">
          <div className="sm-subhead">{fieldLabel(t, F["smith.handoff_offerings"])}</div>
          <div className="sm-note-mb">
            {fieldHelp(t, F["smith.handoff_offerings"])}
          </div>
          {enabledOfferings.length === 0 ? (
            <div className="empty-note">{t("smith.reasoning.no_offerings")}</div>
          ) : (
            <div className="sm-stack">
              {active.handoff_offerings.map((id, i) => {
                const o = enabledOfferings.find((x) => x.id === id);
                if (!o) return null;
                return (
                  <div key={id} className="sm-row-sm">
                    <span className="sm-mono-mute">{i + 1}.</span>
                    <span>
                      {o.wire_model} <span className="sm-mute">via {o.provider}</span>
                    </span>
                    {canAdmin && (
                      <span className="sm-push-right">
                        <button className="btn sm sm-btn-xs" aria-label={t("smith.reasoning.move_up", { name: o.wire_model })} onClick={() => moveOffering(id, -1)} disabled={i === 0}>
                          ↑
                        </button>
                        <button className="btn sm sm-btn-xs" aria-label={t("smith.reasoning.move_down", { name: o.wire_model })} onClick={() => moveOffering(id, 1)}
                          disabled={i === active.handoff_offerings.length - 1}
                        >
                          ↓
                        </button>
                        <button className="btn sm sm-btn-xs" onClick={() => toggleOffering(id)}>
                          {t("smith.reasoning.remove")}
                        </button>
                      </span>
                    )}
                  </div>
                );
              })}
              {canAdmin && enabledOfferings.some((o) => !active.handoff_offerings.includes(o.id)) && (
                <div className="sm-mt-sm">
                  <div className="sm-fine-mb">{t("smith.reasoning.add_label")}</div>
                  <div className="sm-wrap-row">
                    {enabledOfferings
                      .filter((o) => !active.handoff_offerings.includes(o.id))
                      .map((o) => (
                        <button key={o.id} type="button" className="btn sm" onClick={() => toggleOffering(o.id)}>
                          + {o.wire_model}
                        </button>
                      ))}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>

        <div id="smith-brain-chain" className="sm-mt-lg">
          <div className="sm-subhead">{fieldLabel(t, F["smith.brain_chain"])}</div>
          <div className="sm-note-mb">
            {fieldHelp(t, F["smith.brain_chain"])}
          </div>
          {visibleConfigs.length === 0 ? (
            <div className="empty-note">{t("smith.reasoning.no_configs")}</div>
          ) : (
            <div className="sm-stack">
              {active.brain_chain.length === 0 && (
                <div className="sm-note">
                  {t("smith.reasoning.chain_empty")}
                </div>
              )}
              {active.brain_chain.map((name, i) => (
                <div key={name} className="sm-row-sm">
                  <span className="sm-mono-mute">{i + 1}.</span>
                  <span>{name}</span>
                  {canAdmin && (
                    <span className="sm-push-right">
                      <button className="btn sm sm-btn-xs" aria-label={t("smith.reasoning.move_up", { name })} onClick={() => moveBrainChainMember(name, -1)} disabled={i === 0}>
                        ↑
                      </button>
                      <button className="btn sm sm-btn-xs" aria-label={t("smith.reasoning.move_down", { name })} onClick={() => moveBrainChainMember(name, 1)}
                        disabled={i === active.brain_chain.length - 1}
                      >
                        ↓
                      </button>
                      <button className="btn sm sm-btn-xs" onClick={() => toggleBrainChainMember(name)}>
                        {t("smith.reasoning.remove")}
                      </button>
                    </span>
                  )}
                </div>
              ))}
              {canAdmin && visibleConfigs.some((c) => !active.brain_chain.includes(c.name)) && (
                <div className="sm-mt-sm">
                  <div className="sm-fine-mb">{t("smith.reasoning.add_label")}</div>
                  <div className="sm-wrap-row">
                    {visibleConfigs
                      .filter((c) => !active.brain_chain.includes(c.name))
                      .map((c) => (
                        <button key={c.id} type="button" className="btn sm" onClick={() => toggleBrainChainMember(c.name)}>
                          + {c.name}
                        </button>
                      ))}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function ScheduleCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.schedule")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.schedule")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["smith.schedule.quick"]} value={active.schedule.quick} disabled={!canAdmin}
            onChange={(v) => g.setField("schedule", { ...active.schedule, quick: String(v) })} />
          <Field rec={F["smith.schedule.deep"]} value={active.schedule.deep} disabled={!canAdmin}
            onChange={(v) => g.setField("schedule", { ...active.schedule, deep: String(v) })} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function ThresholdsCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.thresholds")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.thresholds")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["smith.thresholds.gtt_warn_pct"]} value={active.thresholds.gtt_warn_pct} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, gtt_warn_pct: Number(v) })} />
          <Field rec={F["smith.thresholds.gtt_crit_pct"]} value={active.thresholds.gtt_crit_pct} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, gtt_crit_pct: Number(v) })} />
          <Field rec={F["smith.thresholds.disk_warn_pct"]} value={active.thresholds.disk_warn_pct} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, disk_warn_pct: Number(v) })} />
          <Field rec={F["smith.thresholds.disk_crit_pct"]} value={active.thresholds.disk_crit_pct} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, disk_crit_pct: Number(v) })} />
          <Field rec={F["smith.thresholds.device_lost_window_minutes"]} value={active.thresholds.device_lost_window_minutes} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, device_lost_window_minutes: Number(v) })} />
          <Field rec={F["smith.thresholds.build_refresh_behind_n"]} value={active.thresholds.build_refresh_behind_n} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, build_refresh_behind_n: Number(v) })} />
          <Field rec={F["smith.thresholds.compressor_failopen_warn_pct"]} value={active.thresholds.compressor_failopen_warn_pct} disabled={!canAdmin}
            onChange={(v) => g.setField("thresholds", { ...active.thresholds, compressor_failopen_warn_pct: Number(v) })} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function WatchlistCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const [draft, setDraft] = useState("");

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.watchlist")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;

  function addKeyword() {
    const kw = draft.trim();
    if (!kw || active!.build_refresh_watchlist.includes(kw)) return;
    g.setField("build_refresh_watchlist", [...active!.build_refresh_watchlist, kw]);
    setDraft("");
  }
  function removeKeyword(kw: string) {
    g.setField("build_refresh_watchlist", active.build_refresh_watchlist.filter((x) => x !== kw));
  }

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.watchlist")}</div>
      <div id="smith-build-refresh-watchlist" className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}
        <div className="sm-note-mb">
          {fieldHelp(t, F["smith.build_refresh_watchlist"])}
        </div>
        {active.build_refresh_watchlist.length === 0 ? (
          <div className="empty-note">{t("smith.watchlist.empty")}</div>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginBottom: canAdmin ? 10 : 0 }}>
            {active.build_refresh_watchlist.map((kw) => (
              <span
                key={kw}
                className="tab sm-inline-mono"
              >
                {kw}
                {canAdmin && (
                  <button
                    aria-label={t("smith.watchlist.remove_aria", { keyword: kw })}
                    onClick={() => removeKeyword(kw)}
                    className="sm-icon-link"
                  >
                    ×
                  </button>
                )}
              </span>
            ))}
          </div>
        )}
        {canAdmin && (
          <div className="sm-gap-row">
            <input
              aria-label={t("smith.watchlist.input_label")}
              placeholder={t("smith.watchlist.placeholder")}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && addKeyword()}
              className="sm-grow"
            />
            <button className="btn" disabled={!draft.trim()} onClick={addKeyword}>{t("smith.watchlist.add")}</button>
          </div>
        )}
        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

const WEB_PROVIDER_ORDER_NAMES = ["searxng", "customsearch", "firecrawl", "customfetch", "direct"] as const;

function providerDotColor(reachable: boolean, checkedAt: string | null): string {
  if (!checkedAt) return "var(--text-mute)"; // never probed — D4: unknown ≠ unreachable
  return reachable ? "var(--ok)" : "var(--crit)";
}

function WebResearchCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();
  const probe = useSmithWebProbe();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.web_research")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const webCfg = active.web;

  function setWeb(patch: Partial<SmithWebSettings>) {
    g.setField("web", { ...webCfg, ...patch });
  }
  function setProvider(name: "searxng" | "customsearch" | "firecrawl" | "customfetch" | "direct", patch: Partial<SmithWebProviderSettings>) {
    setWeb({ [name]: { ...webCfg[name], ...patch } } as Partial<SmithWebSettings>);
  }
  function moveProvider(name: string, dir: -1 | 1) {
    const arr = [...webCfg.provider_order];
    const i = arr.indexOf(name);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= arr.length) return;
    [arr[i], arr[j]] = [arr[j], arr[i]];
    setWeb({ provider_order: arr });
  }

  const statusByName = new Map((status.data?.web.providers ?? []).map((p) => [p.name, p]));
  // provider_order only ever contains the names above (validated server-side);
  // anything unrecognized (a future adapter this build doesn't know) is
  // appended at the end rather than silently dropped from the reorder list.
  const orderedNames = [
    ...webCfg.provider_order,
    ...WEB_PROVIDER_ORDER_NAMES.filter((n) => !webCfg.provider_order.includes(n)),
  ];

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.web_research")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}

        <label className="form-row sm-check-row">
          <input type="checkbox" checked={webCfg.enabled} disabled={!canAdmin}
            onChange={(e) => setWeb({ enabled: e.target.checked })} />
          {fieldLabel(t, F["smith.web.enabled"])}
        </label>
        <div className="sm-note-lead">
          {fieldHelp(t, F["smith.web.enabled"])}
        </div>

        <div id="smith-web-order" className="sm-mb-lg">
          <div className="sm-subhead">{fieldLabel(t, F["smith.web.provider_order"])}</div>
          <div className="sm-note-mb">
            {fieldHelp(t, F["smith.web.provider_order"])}
          </div>
          <div className="sm-stack">
            {orderedNames.map((name, i) => {
              const ps = statusByName.get(name);
              return (
                <div key={name} className="sm-row-sm">
                  <span className="sm-mono-mute">{i + 1}.</span>
                  <span
                    title={ps ? (ps.checked_at ? t("smith.web.status_detail", { state: ps.reachable ? t("smith.web.reachable") : t("smith.web.unreachable"), detail: ps.detail || t("smith.web.no_detail") }) : t("smith.web.never_probed")) : t("smith.web.unknown")}
                    style={{
                      display: "inline-block", width: 8, height: 8, borderRadius: "50%",
                      background: ps ? providerDotColor(ps.reachable, ps.checked_at) : "var(--text-mute)",
                    }}
                  />
                  <span>{name}</span>
                  {name === "direct" && (
                    <span className="sm-fine">{t("smith.web.direct_note")}</span>
                  )}
                  {canAdmin && name !== "direct" && (
                    <span className="sm-push-right">
                      <button className="btn sm sm-btn-xs" aria-label={t("smith.reasoning.move_up", { name })} onClick={() => moveProvider(name, -1)} disabled={i === 0}>↑</button>
                      <button className="btn sm sm-btn-xs" aria-label={t("smith.reasoning.move_down", { name })} onClick={() => moveProvider(name, 1)} disabled={i >= orderedNames.length - 2}>↓</button>
                    </span>
                  )}
                </div>
              );
            })}
          </div>
        </div>

        <div id="smith-web-keys" className="sm-stack-lg">
          <div className="sm-strong">{fieldLabel(t, F["smith.web.api_keys"])}</div>
          <div className="sm-note">{fieldHelp(t, F["smith.web.api_keys"])}</div>
          {(["searxng", "customsearch", "firecrawl", "customfetch"] as const).map((name) => (
            <div key={name} className="sm-list-item">
              <div className="sm-row-md">
                <strong className="sm-cap">{name}</strong>
                {(name === "customsearch" || name === "customfetch") && (
                  <span className="sm-micro">
                    {name === "customsearch" ? t("smith.web.customsearch_note") : t("smith.web.customfetch_note")}
                  </span>
                )}
                <label className="sm-push-right-sm">
                  <input type="checkbox" checked={webCfg[name].enabled} disabled={!canAdmin}
                    onChange={(e) => setProvider(name, { enabled: e.target.checked })} />
                  {t("smith.web.enabled_label")}
                </label>
              </div>
              <input type="text" aria-label={t("smith.web.url_label", { provider: name })} placeholder={t("smith.web.url_placeholder")} value={webCfg[name].base_url} disabled={!canAdmin}
                onChange={(e) => setProvider(name, { base_url: e.target.value })} />
              <input
                type="password" aria-label={t("smith.web.key_label", { provider: name })} placeholder={t("smith.web.key_placeholder")} value={webCfg[name].api_key}
                disabled={!canAdmin} autoComplete="off" spellCheck={false}
                onChange={(e) => setProvider(name, { api_key: e.target.value })}
              />
            </div>
          ))}
        </div>

        <Field rec={F["smith.web.cache_ttl"]} value={webCfg.cache_ttl} disabled={!canAdmin}
          onChange={(v) => setWeb({ cache_ttl: String(v) })} />

        {canAdmin && (
          <div className="form-actions sm-mt">
            <button className="btn" disabled={probe.isPending} onClick={() => probe.mutate()}>
              {probe.isPending ? t("smith.web.probing") : t("smith.web.reprobe")}
            </button>
            {g.dirty && (
              <>
                <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
                <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
              </>
            )}
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// ToolsCard — P7 (docs/v5-smith.md §9): smith.tools. mode "auto" detects
// native-vs-fenced tool calling per brain, optimistic-native, demoting to
// fenced on real evidence (no probe request — never a hardcoded model
// name); "native"/"fenced" pin the mode, the escape hatch a per-model
// verification gap needs; "off" disables the loop entirely. The resolved
// verdict for the CURRENT brain reads off SelfContextChip (GET /status),
// not this form — this form only edits the configured policy.
function ToolsCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.tools")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const tools = active.tools;

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.tools")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}

        <label className="form-row sm-check-row">
          <input type="checkbox" checked={tools.enabled} disabled={!canAdmin}
            onChange={(e) => g.setField("tools", { ...tools, enabled: e.target.checked })} />
          {t("smith.tools.readonly_loop")}
        </label>
        <div className="sm-note-lead">
          {t("smith.tools.description")}
        </div>

        <div className="form-grid">
          <label className="form-row">
            <span>{t("smith.tools.mode_label")}</span>
            <select value={tools.mode} disabled={!canAdmin}
              onChange={(e) => g.setField("tools", { ...tools, mode: e.target.value as typeof tools.mode })}>
              <option value="auto">{t("smith.tools.mode_auto")}</option>
              <option value="native">{t("smith.tools.mode_native")}</option>
              <option value="fenced">{t("smith.tools.mode_fenced")}</option>
              <option value="off">{t("smith.tools.mode_off")}</option>
            </select>
          </label>
          <label className="form-row">
            <span>{t("smith.tools.max_rounds_label")}</span>
            <input
              type="number" min={1} max={8} value={tools.max_rounds} disabled={!canAdmin}
              onChange={(e) => g.setField("tools", { ...tools, max_rounds: Number(e.target.value) })}
            />
          </label>
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// RetentionCard — P7: smith.retention. A *_days field of 0 skips that tier
// entirely (kept, never pruned) rather than deleting everything — the same
// footgun-avoidance as the Cost tab's metrics retention.
function RetentionCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.retention")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const r = active.retention;
  const set = (patch: Partial<typeof r>) => g.setField("retention", { ...r, ...patch });

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.retention")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}

        <label className="form-row sm-check-row">
          <input type="checkbox" checked={r.enabled} disabled={!canAdmin}
            onChange={(e) => set({ enabled: e.target.checked })} />
          {t("smith.retention.prune_label")}
        </label>
        <div className="sm-note-tight">
          {t("smith.retention.description")}
        </div>
        <div className="sm-note-mb-lg">
          {status.data?.retention.last_run_at ? (
            t("smith.retention.last_run", {
              when: new Date(status.data.retention.last_run_at * 1000).toLocaleString(appLocale()),
              findings: status.data.retention.deleted_findings,
              cache: status.data.retention.deleted_web_cache,
            })
          ) : (
            t("smith.retention.never_run")
          )}
        </div>

        <div className="form-grid">
          <label className="form-row">
            <span>{t("smith.retention.ok_days")}</span>
            <input type="number" min={0} value={r.ok_days} disabled={!canAdmin}
              onChange={(e) => set({ ok_days: Number(e.target.value) })} />
          </label>
          <label className="form-row">
            <span>{t("smith.retention.info_hours")}</span>
            <input type="number" min={0} value={r.info_hours} disabled={!canAdmin}
              onChange={(e) => set({ info_hours: Number(e.target.value) })} />
          </label>
          <label className="form-row">
            <span>{t("smith.retention.warn_crit_days")}</span>
            <input type="number" min={0} value={r.warn_crit_days} disabled={!canAdmin}
              onChange={(e) => set({ warn_crit_days: Number(e.target.value) })} />
          </label>
          <label className="form-row">
            <span>{t("smith.retention.web_cache_days")}</span>
            <input type="number" min={0} value={r.web_cache_days} disabled={!canAdmin}
              onChange={(e) => set({ web_cache_days: Number(e.target.value) })} />
          </label>
          <label className="form-row">
            <span>{t("smith.retention.web_cache_max_rows")}</span>
            <input type="number" min={0} value={r.web_cache_max_rows} disabled={!canAdmin}
              onChange={(e) => set({ web_cache_max_rows: Number(e.target.value) })} />
          </label>
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// SelfReviewCard — Thread C's periodic self-review sweep
// (docs/v5-smith-efficiency.md §4): re-checks open investigations and
// parked done_unverified/pending actions on a fixed interval. Investigation
// closures are always a *proposal* (approve it in the pending tray to
// actually close), never auto-resolved here — this card only shows whether
// the sweep is running and what it's found, same posture as RetentionCard.
function SelfReviewCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.self_review")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const sr = active.self_review;
  const set = (patch: Partial<typeof sr>) => g.setField("self_review", { ...sr, ...patch });

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.self_review")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}

        <label className="form-row sm-check-row">
          <input type="checkbox" checked={sr.enabled} disabled={!canAdmin}
            onChange={(e) => set({ enabled: e.target.checked })} />
          {t("smith.self_review.toggle_label")}
        </label>
        <div className="sm-note-tight">
          {t("smith.self_review.description")}
        </div>
        <div className="sm-note-mb-lg">
          {status.data?.self_review.last_run_at ? (
            t("smith.self_review.last_run", {
              when: new Date(status.data.self_review.last_run_at * 1000).toLocaleString(appLocale()),
              promoted: status.data.self_review.actions_promoted,
              superseded: status.data.self_review.actions_superseded,
              proposed: status.data.self_review.investigations_proposed,
            })
          ) : (
            t("smith.self_review.never_run")
          )}
        </div>

        <div className="form-grid">
          <label className="form-row">
            <span>{t("smith.self_review.grace_minutes")}</span>
            <input type="number" min={0} value={sr.grace_minutes} disabled={!canAdmin}
              onChange={(e) => set({ grace_minutes: Number(e.target.value) })} />
          </label>
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// BrainResidencyCard — the opt-in "keep the brain always loaded" choice
// (smith efficiency initiative, Sprint 6 follow-up). Default off: a
// resolvable-but-unloaded brain is loaded on demand, only when a question
// actually escalates to the reasoning tier, and left to unload afterward
// per normal idle policy — this toggle is for operators who'd rather pay
// the VRAM permanently than the load latency per escalated turn.
function BrainResidencyCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.brain_residency")}</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const br = active.brain_residency;
  const set = (patch: Partial<typeof br>) => g.setField("brain_residency", { ...br, ...patch });
  const res = status.data?.brain_residency;

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.brain_residency")}</div>
      <div className="card">
        {g.error && <div className="error-note sm-mb">{g.error}</div>}

        <label className="form-row sm-check-row">
          <input type="checkbox" checked={br.stay_resident} disabled={!canAdmin}
            onChange={(e) => set({ stay_resident: e.target.checked })} />
          {t("smith.brain_residency.toggle_label")}
        </label>
        <div className="sm-note-tight">
          {t("smith.brain_residency.description")}
        </div>
        <div className="sm-note-mb-lg">
          {res?.last_attempt_at ? (
            t("smith.brain_residency.last_attempt", {
              when: new Date(res.last_attempt_at * 1000).toLocaleString(appLocale()),
              result: res.last_loaded
                ? t("smith.brain_residency.succeeded", { slot: res.last_slot ?? "?" })
                : t("smith.brain_residency.failed", { error: res.last_error ?? t("smith.brain_residency.unknown_error") }),
            })
          ) : (
            t("smith.brain_residency.never_attempted")
          )}
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions sm-mt">
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// AutonomyCard — the standing autonomy policy (autonomous-remediation
// Sprint 5, docs/v5-smith.md §13.3). Not a useSettingsGroup card: it's a
// separate endpoint (GET/PUT /api/v1/smith/autonomy, not a field on
// smith/settings) with its own conditional step-up gate — escalating the
// policy (turning the global switch on, or opting a procedure in) requires
// step-up, lowering it never does, so this card drives useStepUpGate
// directly rather than through useSettingsGroup's generic wiring.
function AutonomyCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const policyQuery = useSmithAutonomy();
  const update = useUpdateSmithAutonomy();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<SmithAutonomyPolicy | null>(null);

  if (policyQuery.isError || !policyQuery.data) {
    return (
      <>
        <div className="eyebrow">{t("smith.eyebrow.autonomy")}</div>
        <div className="card">
          <div className="empty-note">{policyQuery.isError ? t("shared.role_required", { resource: "smith" }) : t("common:app.loading")}</div>
        </div>
      </>
    );
  }

  const eligible = policyQuery.data.eligible_procedures;
  const current: SmithAutonomyPolicy = draft ?? policyQuery.data.policy;
  const dirty = draft != null;

  function setPolicy(patch: Partial<SmithAutonomyPolicy>) {
    setDraft({ ...current, ...patch });
  }
  function setProcedure(id: string, patch: Partial<SmithAutonomyProcedurePolicy>) {
    const existing = current.procedures[id] ?? { enabled: false, cooldown_seconds: 0, max_per_day: 0 };
    setPolicy({ procedures: { ...current.procedures, [id]: { ...existing, ...patch } } });
  }

  function save() {
    if (!draft) return;
    setError(null);
    const doSave = () =>
      update.mutate(draft, {
        onSuccess: () => setDraft(null),
        onError: (e) => { if (!gate.handle(e, doSave)) setError(apiErrorMessage(e)); },
      });
    doSave();
  }

  return (
    <>
      <div className="eyebrow">{t("smith.eyebrow.autonomy")}</div>
      <div className="card">
        <div className="sm-note-body">
          {t("smith.autonomy.intro")}
        </div>
        {error && <div className="error-note sm-mb">{error}</div>}

        <label className="form-row sm-check-row">
          <input type="checkbox" checked={current.enabled} disabled={!canAdmin}
            onChange={(e) => setPolicy({ enabled: e.target.checked })} />
          <b>{t("smith.autonomy.global_switch")}</b>
        </label>
        <div className="sm-note-lead">
          {t("smith.autonomy.global_switch_note")}
        </div>

        {eligible.length === 0 ? (
          <div className="empty-note">{t("smith.autonomy.none_eligible")}</div>
        ) : (
          eligible.map((p) => {
            const pa = current.procedures[p.id] ?? { enabled: false, cooldown_seconds: 0, max_per_day: 0 };
            return (
              <div key={p.id} className="sm-sep">
                <label className="form-row sm-check-row">
                  <input type="checkbox" checked={pa.enabled} disabled={!canAdmin || !current.enabled}
                    onChange={(e) => setProcedure(p.id, { enabled: e.target.checked })} />
                  {p.title}
                </label>
                <div className="form-row sm-check-row-wide">
                  <label className="sm-note">
                    {t("smith.autonomy.cooldown_label")}
                    <input type="number" min={0} className="sm-w90" disabled={!canAdmin}
                      value={pa.cooldown_seconds || ""} placeholder="600"
                      onChange={(e) => setProcedure(p.id, { cooldown_seconds: Number(e.target.value) || 0 })} />
                  </label>
                  <label className="sm-note">
                    {t("smith.autonomy.max_per_day_label")}
                    <input type="number" min={0} className="sm-w70" disabled={!canAdmin}
                      value={pa.max_per_day || ""} placeholder="3"
                      onChange={(e) => setProcedure(p.id, { max_per_day: Number(e.target.value) || 0 })} />
                  </label>
                </div>
              </div>
            );
          })
        )}

        {canAdmin && dirty && (
          <div className="form-actions sm-mt-lg">
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>{t("shared.reset")}</button>
            <button className="btn primary" disabled={update.isPending} onClick={save}>{t("smith.autonomy.save_policy")}</button>
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

export function Smith({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <ReasoningCard canAdmin={canAdmin} />
      <ScheduleCard canAdmin={canAdmin} />
      <ThresholdsCard canAdmin={canAdmin} />
      <WatchlistCard canAdmin={canAdmin} />
      <WebResearchCard canAdmin={canAdmin} />
      <ToolsCard canAdmin={canAdmin} />
      <RetentionCard canAdmin={canAdmin} />
      <SelfReviewCard canAdmin={canAdmin} />
      <BrainResidencyCard canAdmin={canAdmin} />
      <AutonomyCard canAdmin={canAdmin} />
    </>
  );
}
