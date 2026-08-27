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
import { apiErrorMessage } from "../../lib/api";
import { formatGB } from "../../lib/format";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { useCatalogConfigs, useCatalogOfferings, useSmithAutonomy, useSmithSettings, useSmithSourcingEvaluate, useSmithStatus, useSmithWebProbe, useUpdateSmithAutonomy, useUpdateSmithSettings } from "../../lib/queries";
import type { SmithAutonomyPolicy, SmithAutonomyProcedurePolicy, SmithSourcingEvaluation, SmithWebProviderSettings, SmithWebSettings } from "../../lib/types";
import { Field } from "../Field";
import { SMITH_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";
import { useStepUpGate } from "../../lib/useStepUpGate";

const F = Object.fromEntries(SMITH_FIELDS.map((f) => [f.id, f]));

function ReasoningCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const configs = useCatalogConfigs();
  const offerings = useCatalogOfferings();

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">smith → Reasoning</div>
        <div className="card"><div className="empty-note">Operator role required to view smith settings.</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">smith → Reasoning</div>
        <div className="card"><div className="empty-note">Loading smith settings…</div></div>
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
      <div className="eyebrow">smith → Reasoning</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}

        <div id="smith-model" className="form-row">
          <span style={{ display: "flex", alignItems: "center", gap: 6 }}>{F["smith.model"].label}</span>
          <select value={active.model} disabled={!canAdmin} onChange={(e) => g.setField("model", e.target.value)}>
            <option value="">— none (answers without a model) —</option>
            {visibleConfigs.length > 0 && (
              <optgroup label="Local configs">
                {visibleConfigs.map((c) => (
                  <option key={`c${c.id}`} value={c.name}>{c.name}</option>
                ))}
              </optgroup>
            )}
            {enabledOfferings.length > 0 && (
              <optgroup label="Remote offerings">
                {enabledOfferings.map((o) => (
                  <option key={`o${o.id}`} value={o.wire_model}>
                    {o.wire_model} ({o.provider})
                  </option>
                ))}
              </optgroup>
            )}
          </select>
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 14 }}>
          {F["smith.model"].help}
        </div>

        <div id="smith-handoff-offerings">
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>{F["smith.handoff_offerings"].label}</div>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
            {F["smith.handoff_offerings"].help}
          </div>
          {enabledOfferings.length === 0 ? (
            <div className="empty-note">No enabled remote offerings to choose from.</div>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
              {active.handoff_offerings.map((id, i) => {
                const o = enabledOfferings.find((x) => x.id === id);
                if (!o) return null;
                return (
                  <div key={id} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11.5 }}>
                    <span style={{ fontFamily: "var(--mono)", color: "var(--text-mute)" }}>{i + 1}.</span>
                    <span>
                      {o.wire_model} <span style={{ color: "var(--text-mute)" }}>via {o.provider}</span>
                    </span>
                    {canAdmin && (
                      <span style={{ marginLeft: "auto", display: "flex", gap: 4 }}>
                        <button className="tab" style={{ padding: "1px 6px" }} onClick={() => moveOffering(id, -1)} disabled={i === 0}>
                          ↑
                        </button>
                        <button
                          className="tab" style={{ padding: "1px 6px" }} onClick={() => moveOffering(id, 1)}
                          disabled={i === active.handoff_offerings.length - 1}
                        >
                          ↓
                        </button>
                        <button className="tab" style={{ padding: "1px 6px" }} onClick={() => toggleOffering(id)}>
                          remove
                        </button>
                      </span>
                    )}
                  </div>
                );
              })}
              {canAdmin && enabledOfferings.some((o) => !active.handoff_offerings.includes(o.id)) && (
                <div style={{ marginTop: 6 }}>
                  <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 4 }}>Add:</div>
                  <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                    {enabledOfferings
                      .filter((o) => !active.handoff_offerings.includes(o.id))
                      .map((o) => (
                        <button key={o.id} className="tab" onClick={() => toggleOffering(o.id)}>
                          + {o.wire_model}
                        </button>
                      ))}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>

        <div id="smith-brain-chain" style={{ marginTop: 14 }}>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>{F["smith.brain_chain"].label}</div>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
            {F["smith.brain_chain"].help}
          </div>
          {visibleConfigs.length === 0 ? (
            <div className="empty-note">No local configs to choose from.</div>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
              {active.brain_chain.length === 0 && (
                <div style={{ fontSize: 11, color: "var(--text-mute)" }}>
                  Empty — smith escalates straight to smith.model above, no chain.
                </div>
              )}
              {active.brain_chain.map((name, i) => (
                <div key={name} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11.5 }}>
                  <span style={{ fontFamily: "var(--mono)", color: "var(--text-mute)" }}>{i + 1}.</span>
                  <span>{name}</span>
                  {canAdmin && (
                    <span style={{ marginLeft: "auto", display: "flex", gap: 4 }}>
                      <button className="tab" style={{ padding: "1px 6px" }} onClick={() => moveBrainChainMember(name, -1)} disabled={i === 0}>
                        ↑
                      </button>
                      <button
                        className="tab" style={{ padding: "1px 6px" }} onClick={() => moveBrainChainMember(name, 1)}
                        disabled={i === active.brain_chain.length - 1}
                      >
                        ↓
                      </button>
                      <button className="tab" style={{ padding: "1px 6px" }} onClick={() => toggleBrainChainMember(name)}>
                        remove
                      </button>
                    </span>
                  )}
                </div>
              ))}
              {canAdmin && visibleConfigs.some((c) => !active.brain_chain.includes(c.name)) && (
                <div style={{ marginTop: 6 }}>
                  <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 4 }}>Add:</div>
                  <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                    {visibleConfigs
                      .filter((c) => !active.brain_chain.includes(c.name))
                      .map((c) => (
                        <button key={c.id} className="tab" onClick={() => toggleBrainChainMember(c.name)}>
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
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function ScheduleCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Schedule</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
        </div>
      </>
    );
  }
  const active = g.active;

  return (
    <>
      <div className="eyebrow">smith → Schedule</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["smith.schedule.quick"]} value={active.schedule.quick} disabled={!canAdmin}
            onChange={(v) => g.setField("schedule", { ...active.schedule, quick: String(v) })} />
          <Field rec={F["smith.schedule.deep"]} value={active.schedule.deep} disabled={!canAdmin}
            onChange={(v) => g.setField("schedule", { ...active.schedule, deep: String(v) })} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function ThresholdsCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Thresholds</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
        </div>
      </>
    );
  }
  const active = g.active;

  return (
    <>
      <div className="eyebrow">smith → Thresholds</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
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
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function WatchlistCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const [draft, setDraft] = useState("");

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Build-refresh watchlist</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
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
      <div className="eyebrow">smith → Build-refresh watchlist</div>
      <div id="smith-build-refresh-watchlist" className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
          {F["smith.build_refresh_watchlist"].help}
        </div>
        {active.build_refresh_watchlist.length === 0 ? (
          <div className="empty-note">Empty — no commit-subject matching happens.</div>
        ) : (
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginBottom: canAdmin ? 10 : 0 }}>
            {active.build_refresh_watchlist.map((kw) => (
              <span
                key={kw}
                className="tab"
                style={{ display: "inline-flex", alignItems: "center", gap: 6, fontFamily: "var(--mono)", fontSize: 11.5 }}
              >
                {kw}
                {canAdmin && (
                  <button
                    aria-label={`remove ${kw}`}
                    onClick={() => removeKeyword(kw)}
                    style={{ background: "none", border: "none", color: "var(--text-mute)", cursor: "pointer", padding: 0, fontSize: 13, lineHeight: 1 }}
                  >
                    ×
                  </button>
                )}
              </span>
            ))}
          </div>
        )}
        {canAdmin && (
          <div style={{ display: "flex", gap: 6 }}>
            <input
              placeholder="e.g. CVE, breaking, security"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && addKeyword()}
              style={{ flex: 1 }}
            />
            <button className="btn" disabled={!draft.trim()} onClick={addKeyword}>+ Add</button>
          </div>
        )}
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
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
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();
  const probe = useSmithWebProbe();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Web research</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
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
      <div className="eyebrow">smith → Web research</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}

        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input type="checkbox" checked={webCfg.enabled} disabled={!canAdmin}
            onChange={(e) => setWeb({ enabled: e.target.checked })} />
          {F["smith.web.enabled"].label}
        </label>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 14 }}>
          {F["smith.web.enabled"].help}
        </div>

        <div id="smith-web-order" style={{ marginBottom: 14 }}>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>{F["smith.web.provider_order"].label}</div>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
            {F["smith.web.provider_order"].help}
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            {orderedNames.map((name, i) => {
              const ps = statusByName.get(name);
              return (
                <div key={name} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11.5 }}>
                  <span style={{ fontFamily: "var(--mono)", color: "var(--text-mute)" }}>{i + 1}.</span>
                  <span
                    title={ps ? (ps.checked_at ? `${ps.reachable ? "reachable" : "unreachable"} — ${ps.detail || "no detail"}` : "never probed") : "unknown"}
                    style={{
                      display: "inline-block", width: 8, height: 8, borderRadius: "50%",
                      background: ps ? providerDotColor(ps.reachable, ps.checked_at) : "var(--text-mute)",
                    }}
                  />
                  <span>{name}</span>
                  {name === "direct" && (
                    <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>(always tried last, regardless of order)</span>
                  )}
                  {canAdmin && name !== "direct" && (
                    <span style={{ marginLeft: "auto", display: "flex", gap: 4 }}>
                      <button className="tab" style={{ padding: "1px 6px" }} onClick={() => moveProvider(name, -1)} disabled={i === 0}>↑</button>
                      <button className="tab" style={{ padding: "1px 6px" }} onClick={() => moveProvider(name, 1)} disabled={i >= orderedNames.length - 2}>↓</button>
                    </span>
                  )}
                </div>
              );
            })}
          </div>
        </div>

        <div id="smith-web-keys" style={{ display: "flex", flexDirection: "column", gap: 10, marginBottom: 14 }}>
          <div style={{ fontSize: 12, fontWeight: 600 }}>{F["smith.web.api_keys"].label}</div>
          <div style={{ fontSize: 11, color: "var(--text-mute)" }}>{F["smith.web.api_keys"].help}</div>
          {(["searxng", "customsearch", "firecrawl", "customfetch"] as const).map((name) => (
            <div key={name} style={{ display: "flex", flexDirection: "column", gap: 4, padding: "8px 0", borderTop: "1px solid var(--border)" }}>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                <strong style={{ fontSize: 12, textTransform: "capitalize" }}>{name}</strong>
                {(name === "customsearch" || name === "customfetch") && (
                  <span style={{ fontSize: 10, color: "var(--text-mute)" }}>
                    {name === "customsearch" ? "(SearxNG-compatible GET ?q= endpoint)" : "(GET ?url= text proxy)"}
                  </span>
                )}
                <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 11, marginLeft: "auto" }}>
                  <input type="checkbox" checked={webCfg[name].enabled} disabled={!canAdmin}
                    onChange={(e) => setProvider(name, { enabled: e.target.checked })} />
                  enabled
                </label>
              </div>
              <input type="text" placeholder="https://…" value={webCfg[name].base_url} disabled={!canAdmin}
                onChange={(e) => setProvider(name, { base_url: e.target.value })} />
              <input
                type="password" placeholder="API key (leave as-is to keep unchanged)" value={webCfg[name].api_key}
                disabled={!canAdmin} autoComplete="off" spellCheck={false}
                onChange={(e) => setProvider(name, { api_key: e.target.value })}
              />
            </div>
          ))}
        </div>

        <Field rec={F["smith.web.cache_ttl"]} value={webCfg.cache_ttl} disabled={!canAdmin}
          onChange={(v) => setWeb({ cache_ttl: String(v) })} />

        {canAdmin && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" disabled={probe.isPending} onClick={() => probe.mutate()}>
              {probe.isPending ? "Probing…" : "Re-probe now"}
            </button>
            {g.dirty && (
              <>
                <button className="btn" onClick={g.reset}>Reset</button>
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
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Tools</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const tools = active.tools;

  return (
    <>
      <div className="eyebrow">smith → Tools</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}

        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input type="checkbox" checked={tools.enabled} disabled={!canAdmin}
            onChange={(e) => g.setField("tools", { ...tools, enabled: e.target.checked })} />
          Read-only tool loop
        </label>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 14 }}>
          Lets the reasoning brain call smith's own checks, findings, knowledge base, catalog, and
          (when web research is enabled) web search/fetch mid-conversation instead of only answering
          from the context snapshot. Never mutates anything — every tool is a read.
        </div>

        <div className="form-grid">
          <div className="form-row">
            <span>Mode</span>
            <select value={tools.mode} disabled={!canAdmin}
              onChange={(e) => g.setField("tools", { ...tools, mode: e.target.value as typeof tools.mode })}>
              <option value="auto">auto — detect per brain (recommended)</option>
              <option value="native">native — force OpenAI tool_calls</option>
              <option value="fenced">fenced — force the ```tool_call fallback</option>
              <option value="off">off</option>
            </select>
          </div>
          <div className="form-row">
            <span>Max rounds</span>
            <input
              type="number" min={1} max={8} value={tools.max_rounds} disabled={!canAdmin}
              onChange={(e) => g.setField("tools", { ...tools, max_rounds: Number(e.target.value) })}
            />
          </div>
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
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
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Retention</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const r = active.retention;
  const set = (patch: Partial<typeof r>) => g.setField("retention", { ...r, ...patch });

  return (
    <>
      <div className="eyebrow">smith → Retention</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}

        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input type="checkbox" checked={r.enabled} disabled={!canAdmin}
            onChange={(e) => set({ enabled: e.target.checked })} />
          Prune old findings + web cache automatically
        </label>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 8 }}>
          A finding attached to an open or closed investigation is never pruned, at any age, regardless
          of these settings — only standalone sweep findings age out. 0 keeps a tier forever.
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 14 }}>
          {status.data?.retention.last_run_at ? (
            <>
              Last run {new Date(status.data.retention.last_run_at * 1000).toLocaleString()} — deleted{" "}
              {status.data.retention.deleted_findings} findings, {status.data.retention.deleted_web_cache} web
              cache rows.
            </>
          ) : (
            "No prune has run yet — runs automatically every 6h once smith is started."
          )}
        </div>

        <div className="form-grid">
          <div className="form-row">
            <span>"ok" findings, days</span>
            <input type="number" min={0} value={r.ok_days} disabled={!canAdmin}
              onChange={(e) => set({ ok_days: Number(e.target.value) })} />
          </div>
          <div className="form-row">
            <span>"info" findings, hours</span>
            <input type="number" min={0} value={r.info_hours} disabled={!canAdmin}
              onChange={(e) => set({ info_hours: Number(e.target.value) })} />
          </div>
          <div className="form-row">
            <span>"warn"/"crit" findings, days</span>
            <input type="number" min={0} value={r.warn_crit_days} disabled={!canAdmin}
              onChange={(e) => set({ warn_crit_days: Number(e.target.value) })} />
          </div>
          <div className="form-row">
            <span>Web cache, days</span>
            <input type="number" min={0} value={r.web_cache_days} disabled={!canAdmin}
              onChange={(e) => set({ web_cache_days: Number(e.target.value) })} />
          </div>
          <div className="form-row">
            <span>Web cache, max rows</span>
            <input type="number" min={0} value={r.web_cache_max_rows} disabled={!canAdmin}
              onChange={(e) => set({ web_cache_max_rows: Number(e.target.value) })} />
          </div>
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
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
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Self-review</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
        </div>
      </>
    );
  }
  const active = g.active;
  const sr = active.self_review;
  const set = (patch: Partial<typeof sr>) => g.setField("self_review", { ...sr, ...patch });

  return (
    <>
      <div className="eyebrow">smith → Self-review</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}

        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input type="checkbox" checked={sr.enabled} disabled={!canAdmin}
            onChange={(e) => set({ enabled: e.target.checked })} />
          Periodically re-check open investigations and parked actions
        </label>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 8 }}>
          An investigation whose checks now read clean gets a closure proposal in the pending
          tray — approve it to actually close the investigation. A stuck "done, unverified"
          action is promoted to "done" automatically once its own checks re-verify clean (that's
          bookkeeping catching up to reality, not a new action); a pending proposal smith made on
          its own is superseded automatically once the condition it was about no longer applies.
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 14 }}>
          {status.data?.self_review.last_run_at ? (
            <>
              Last run {new Date(status.data.self_review.last_run_at * 1000).toLocaleString()} —
              promoted {status.data.self_review.actions_promoted} action(s), superseded{" "}
              {status.data.self_review.actions_superseded}, proposed{" "}
              {status.data.self_review.investigations_proposed} investigation closure(s).
            </>
          ) : (
            "No self-review sweep has run yet — runs automatically every 2h once smith is started."
          )}
        </div>

        <div className="form-grid">
          <div className="form-row">
            <span>Grace period, minutes</span>
            <input type="number" min={0} value={sr.grace_minutes} disabled={!canAdmin}
              onChange={(e) => set({ grace_minutes: Number(e.target.value) })} />
          </div>
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
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
  const cfg = useSmithSettings();
  const update = useUpdateSmithSettings();
  const g = useSettingsGroup(cfg.data, update);
  const status = useSmithStatus();

  if (cfg.isError || !g.active) {
    return (
      <>
        <div className="eyebrow">smith → Brain residency</div>
        <div className="card">
          <div className="empty-note">{cfg.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
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
      <div className="eyebrow">smith → Brain residency</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}

        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input type="checkbox" checked={br.stay_resident} disabled={!canAdmin}
            onChange={(e) => set({ stay_resident: e.target.checked })} />
          Keep smith's brain always loaded
        </label>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 8 }}>
          Off (default): smith's brain is loaded on demand, only when a question actually needs
          the reasoning tier, and left to unload afterward per normal idle policy — no standing
          VRAM cost, but each first escalated question after an idle period pays a real load
          delay. On: smith proactively keeps the brain resident, permanently giving up its VRAM
          footprint in exchange for no per-question load latency. Either way, the brain is never
          pinned to a specific slot — the scheduler picks whichever is free.
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 14 }}>
          {res?.last_attempt_at ? (
            <>
              Last load attempt {new Date(res.last_attempt_at * 1000).toLocaleString()} —{" "}
              {res.last_loaded ? `succeeded (slot ${res.last_slot ?? "?"})` : `failed (${res.last_error ?? "unknown error"})`}.
            </>
          ) : (
            "No load attempt yet."
          )}
        </div>

        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
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
  const policyQuery = useSmithAutonomy();
  const update = useUpdateSmithAutonomy();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<SmithAutonomyPolicy | null>(null);

  if (policyQuery.isError || !policyQuery.data) {
    return (
      <>
        <div className="eyebrow">smith → Standing autonomy policy</div>
        <div className="card">
          <div className="empty-note">{policyQuery.isError ? "Operator role required to view smith settings." : "Loading…"}</div>
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
      <div className="eyebrow">smith → Standing autonomy policy</div>
      <div className="card">
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 12, lineHeight: 1.55 }}>
          Off by default. When enabled here, smith may carry out a specific, opted-in fix the
          moment it detects the problem — no click, no approval — instead of only ever proposing
          it for a human. Every autonomous run still goes through the same procedure engine
          (checkpoints, post-verify, full audit trail) as a manually-approved one, and is bounded
          by a cooldown plus a daily cap per procedure. Only procedures assessed as low-risk are
          ever eligible for this — the list below can't be widened from here.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input type="checkbox" checked={current.enabled} disabled={!canAdmin}
            onChange={(e) => setPolicy({ enabled: e.target.checked })} />
          <b>Global switch — smith may run opted-in fixes unattended</b>
        </label>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 2, marginBottom: 14 }}>
          The kill switch. Off always means nothing is autonomous, regardless of the per-procedure
          toggles below.
        </div>

        {eligible.length === 0 ? (
          <div className="empty-note">No procedures are currently eligible for standing autonomy.</div>
        ) : (
          eligible.map((p) => {
            const pa = current.procedures[p.id] ?? { enabled: false, cooldown_seconds: 0, max_per_day: 0 };
            return (
              <div key={p.id} style={{ borderTop: "1px solid var(--border)", paddingTop: 10, marginTop: 10 }}>
                <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
                  <input type="checkbox" checked={pa.enabled} disabled={!canAdmin || !current.enabled}
                    onChange={(e) => setProcedure(p.id, { enabled: e.target.checked })} />
                  {p.title}
                </label>
                <div className="form-row" style={{ flexDirection: "row", gap: 16, marginTop: 6 }}>
                  <label style={{ fontSize: 11, color: "var(--text-mute)" }}>
                    Cooldown (seconds)
                    <input type="number" min={0} style={{ width: 90, marginLeft: 6 }} disabled={!canAdmin}
                      value={pa.cooldown_seconds || ""} placeholder="600"
                      onChange={(e) => setProcedure(p.id, { cooldown_seconds: Number(e.target.value) || 0 })} />
                  </label>
                  <label style={{ fontSize: 11, color: "var(--text-mute)" }}>
                    Max runs / day
                    <input type="number" min={0} style={{ width: 70, marginLeft: 6 }} disabled={!canAdmin}
                      value={pa.max_per_day || ""} placeholder="3"
                      onChange={(e) => setProcedure(p.id, { max_per_day: Number(e.target.value) || 0 })} />
                  </label>
                </div>
              </div>
            );
          })
        )}

        {canAdmin && dirty && (
          <div className="form-actions" style={{ marginTop: 14 }}>
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>Reset</button>
            <button className="btn primary" disabled={update.isPending} onClick={save}>Save policy</button>
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// SourcingCard — P6 FR4 (docs/v5-smith.md §4.9). Not a useSettingsGroup
// card like the four above it (there's nothing to save — smith.sourcing.
// enabled is the only persisted key, and it's a simple on/off with no
// dedicated UI need yet); this is a one-shot research tool: enter a HF
// repo, get back ranked GGUF candidates against a memory budget. A budget
// of 0 (left blank) tells the backend to fall back to the live collector
// snapshot's GTT total (smith.Evaluate's own default).
function SourcingCard() {
  const [repo, setRepo] = useState("");
  const [budgetGB, setBudgetGB] = useState("");
  const [result, setResult] = useState<SmithSourcingEvaluation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const evaluate = useSmithSourcingEvaluate();

  function run() {
    setError(null);
    setResult(null);
    const budgetBytes = budgetGB.trim() ? Math.round(parseFloat(budgetGB) * (1024 ** 3)) : undefined;
    evaluate.mutate(
      { hfRepo: repo.trim(), budgetBytes },
      {
        onSuccess: (data) => setResult(data.evaluation),
        onError: (e) => setError(apiErrorMessage(e)),
      },
    );
  }

  return (
    <>
      <div className="eyebrow">smith → Model sourcing</div>
      <div className="card">
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 10 }}>
          Evaluate a HuggingFace repo's real GGUF files against a memory budget — ranked by the
          documented 1.2× VRAM rule and quant-preference heuristics (modelselection.md). Read-only;
          nothing is downloaded or added to the catalog.
        </div>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
          <label className="form-row" style={{ flex: "1 1 260px" }}>
            <input
              placeholder="org/repo (e.g. Qwen/Qwen2.5-Coder-7B-Instruct-GGUF)"
              value={repo} onChange={(e) => setRepo(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && repo.trim() && run()}
            />
          </label>
          <label className="form-row" style={{ width: 140 }}>
            <input
              placeholder="budget GB (auto)"
              value={budgetGB} onChange={(e) => setBudgetGB(e.target.value)}
            />
          </label>
          <button className="btn primary" disabled={!repo.trim() || evaluate.isPending} onClick={run}>
            {evaluate.isPending ? "Evaluating…" : "Evaluate"}
          </button>
        </div>

        {error && <div className="error-note" style={{ marginTop: 10 }}>{error}</div>}

        {result && (
          <div style={{ marginTop: 12 }}>
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 6 }}>
              {result.repo} — budget {formatGB(result.budget_bytes, 1)} GB
              {result.cached && " (cached)"}
            </div>
            {result.candidates.length === 0 ? (
              <div className="empty-note">No GGUF files found in this repo.</div>
            ) : (
              <table style={{ width: "100%", fontSize: 11.5, borderCollapse: "collapse" }}>
                <thead>
                  <tr style={{ color: "var(--text-mute)", textAlign: "left" }}>
                    <th style={{ padding: "3px 6px", fontWeight: 500 }}>file</th>
                    <th style={{ padding: "3px 6px", fontWeight: 500 }}>quant</th>
                    <th style={{ padding: "3px 6px", fontWeight: 500, textAlign: "right" }}>size</th>
                    <th style={{ padding: "3px 6px", fontWeight: 500, textAlign: "right" }}>est. VRAM</th>
                    <th style={{ padding: "3px 6px", fontWeight: 500 }}>fits</th>
                  </tr>
                </thead>
                <tbody>
                  {result.candidates.map((c) => (
                    <tr key={c.filename} style={c.recommended ? { background: "color-mix(in srgb, var(--ok) 10%, transparent)" } : undefined}>
                      <td style={{ padding: "3px 6px", fontFamily: "var(--mono)", wordBreak: "break-all" }}>
                        {c.recommended && <span title="Recommended">★ </span>}
                        {c.filename}
                      </td>
                      <td style={{ padding: "3px 6px", color: "var(--text-dim)" }}>{c.quant || "—"}</td>
                      <td style={{ padding: "3px 6px", textAlign: "right", color: "var(--text-dim)" }}>
                        {formatGB(c.size_bytes, 1)} GB
                      </td>
                      <td style={{ padding: "3px 6px", textAlign: "right", color: "var(--text-dim)" }}>
                        {formatGB(c.estimated_vram_bytes, 1)} GB
                      </td>
                      <td style={{ padding: "3px 6px", color: c.fits_budget ? "var(--ok)" : "var(--text-mute)" }}>
                        {c.fits_budget ? "yes" : "no"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {result.download_steps.length > 0 && (
              <div style={{ marginTop: 10 }}>
                <div style={{ fontSize: 11, fontWeight: 600, marginBottom: 4 }}>Download the recommended file</div>
                {result.download_steps.map((step, i) => (
                  <pre key={`${step.command}-${i}`} style={{
                    padding: 8, borderRadius: 6, fontSize: 10.5, lineHeight: 1.45,
                    fontFamily: "var(--mono)", background: "var(--bg)", color: "var(--text-dim)",
                    border: "1px solid var(--border)", overflow: "auto", marginBottom: 4,
                  }}>
                    {step.command}
                  </pre>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
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
      <SourcingCard />
    </>
  );
}
