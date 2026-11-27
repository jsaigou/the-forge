// settings/panels/General.tsx — read-only daemon strip and the restart-
// required banner (Status.restart_required — the fuller, always-visible
// counterpart to App.tsx's topbar pip).
import { Trans, useTranslation } from "react-i18next";
import { useSystemSettings, useStatus, useInfraServices, useServiceLinks, useUpdateServiceLinks, useExternalTools, useUpdateExternalTools } from "../../lib/queries";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { type Lang, setLang } from "../../lib/i18n";
import { appLocale } from "../../lib/format";
import { useSettingsGroup } from "../useSettingsGroup";
import type { ExternalTool } from "../../lib/types";

// Per-browser preference, same storage model as App.tsx's theme toggle — not
// synced across devices or operators. See docs/adr/0016-localization.md.
function LanguageCard() {
  const { t, i18n } = useTranslation("common");
  const lang = (i18n.language === "ja" ? "ja" : "en") as Lang;

  return (
    <>
      <div className="eyebrow">{t("language.label")}</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          {t("language.hint")}
        </div>
        <div className="form-grid">
          <label className="form-row">
            {t("language.label")}
            <select value={lang} onChange={(e) => setLang(e.target.value as Lang)}>
              <option value="en">English</option>
              <option value="ja">日本語</option>
            </select>
          </label>
        </div>
      </div>
    </>
  );
}

function RestartBanner() {
  const { t } = useTranslation("settings");
  const status = useStatus();
  const info = status.data?.restart_required;
  if (!info) return null;
  const since = new Date(info.since);
  return (
    <div className="restart-banner">
      <b>{t("general.restart_banner.title")}</b> —{" "}
      <Trans
        i18nKey="general.restart_banner.body"
        ns="settings"
        count={info.keys.length}
        values={{ by: info.by, when: since.toLocaleString(appLocale()), keys: info.keys.join(", ") }}
        components={{ b: <b /> }}
      />{" "}
      {t("general.restart_banner.apply_prefix")}{" "}
      <code>sudo systemctl restart forge-daemon</code>.
    </div>
  );
}

function DaemonStrip({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const status = useStatus();
  // Admin-gated on the backend (GET /api/v1/system/settings) — skip the
  // request entirely for non-admins rather than firing a request that's
  // guaranteed to 403 (see useSystemSettings's doc comment).
  const system = useSystemSettings(canAdmin);

  return (
    <>
      <div className="eyebrow">{t("general.daemon.title")}</div>
      <div className="card">
        <RestartBanner />
        <div className="form-grid">
          <label className="form-row">{t("general.daemon.version")}<input value={status.data?.version ?? "…"} disabled readOnly /></label>
          <label className="form-row">{t("general.daemon.hostname")}<input value={status.data?.hostname ?? "…"} disabled readOnly /></label>
          {canAdmin && system.data && (
            <>
              <label className="form-row">{t("general.daemon.dashboard_listen")}<input value={system.data.listen} disabled readOnly /></label>
              <label className="form-row">{t("general.daemon.a0_listen")}<input value={system.data.router_listen} disabled readOnly /></label>
              <label className="form-row">{t("general.daemon.mcp_listen")}<input value={system.data.mcp_listen} disabled readOnly /></label>
            </>
          )}
        </div>
        {canAdmin && (
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 4 }}>
            {t("general.daemon.readonly_note")}
          </div>
        )}
      </div>
    </>
  );
}

// ServiceLinksCard — Console services strip's ↗ link is guessed as
// `http://<dashboard-host>:<port>` (ServicesBar.tsx's ServiceChip) whenever
// no override exists. That guess is wrong for any service reachable only
// through its own hostname (e.g. ComfyUI, served over its own Tailscale
// Serve endpoint, distinct from the dashboard's own). Operator feedback
// 2026-09-22: the ComfyUI link was wrong and there was no settings surface
// to fix it — this closes that gap for every service, not just ComfyUI.
function ServiceLinksCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const infra = useInfraServices();
  const links = useServiceLinks();
  const update = useUpdateServiceLinks();
  const g = useSettingsGroup(links.data, update);

  const names = Array.from(new Set((infra.data?.services ?? []).filter((svc) => svc.kind !== "external").map((svc) => svc.name))).sort((a, b) =>
    a.localeCompare(b),
  );

  return (
    <>
      <div className="eyebrow">{t("general.service_links.title")}</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          {t("general.service_links.description_before")} <code>http://&lt;dashboard-host&gt;:&lt;port&gt;</code>{" "}
          {t("general.service_links.description_after")}
        </div>
        {links.isError && <div className="empty-note">{t("general.service_links.role_required")}</div>}
        {!links.isError && !g.active && <div className="empty-note">{t("general.service_links.loading")}</div>}
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        {g.active && (
          <div className="form-grid">
            {names.map((name) => (
              <label className="form-row" key={name}>
                {name}
                <input
                  type="text"
                  placeholder={t("general.service_links.placeholder")}
                  value={g.active?.[name] ?? ""}
                  disabled={!canAdmin}
                  onChange={(e) => g.setField(name, e.target.value)}
                />
              </label>
            ))}
            {names.length === 0 && <div className="empty-note">{t("general.service_links.no_services")}</div>}
          </div>
        )}
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// ExternalToolsCard — link-only Console entries (infra.external_tools) for tools that run elsewhere, such as voice-studio and talk-studio on Core. Each is a
// name, a link, an optional icon name and an optional line of description; the Console shows them as chips with a ↗ link and no status.
function ExternalToolsCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const tools = useExternalTools();
  const update = useUpdateExternalTools();
  const g = useSettingsGroup(tools.data, update);
  const list = g.active?.tools ?? [];
  const setTool = (i: number, patch: Partial<ExternalTool>) =>
    g.setField("tools", list.map((x, j) => (j === i ? { ...x, ...patch } : x)));

  return (
    <>
      <div className="eyebrow">{t("general.external_tools.title")}</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>{t("general.external_tools.description")}</div>
        {tools.isError && <div className="empty-note">{t("general.external_tools.role_required")}</div>}
        {!tools.isError && !g.active && <div className="empty-note">{t("general.external_tools.loading")}</div>}
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        {g.active && list.length === 0 && <div className="empty-note">{t("general.external_tools.none")}</div>}
        {g.active &&
          list.map((tool, i) => (
            <div className="form-grid" key={i} style={{ marginBottom: 14, paddingBottom: 14, borderBottom: "1px solid var(--border)" }}>
              <label className="form-row">
                {t("general.external_tools.name")}
                <input type="text" value={tool.name} disabled={!canAdmin} maxLength={64} onChange={(e) => setTool(i, { name: e.target.value })} />
              </label>
              <label className="form-row">
                {t("general.external_tools.url")}
                <input type="text" value={tool.url} disabled={!canAdmin} placeholder={t("general.service_links.placeholder")} onChange={(e) => setTool(i, { url: e.target.value })} />
              </label>
              <label className="form-row">
                {t("general.external_tools.icon")}
                <input type="text" value={tool.icon ?? ""} disabled={!canAdmin} maxLength={64} onChange={(e) => setTool(i, { icon: e.target.value })} />
              </label>
              <label className="form-row">
                {t("general.external_tools.description_field")}
                <input type="text" value={tool.description ?? ""} disabled={!canAdmin} maxLength={200} onChange={(e) => setTool(i, { description: e.target.value })} />
              </label>
              {canAdmin && (
                <div className="form-actions">
                  <button className="btn" onClick={() => g.setField("tools", list.filter((_, j) => j !== i))}>{t("general.external_tools.remove")}</button>
                </div>
              )}
            </div>
          ))}
        {canAdmin && g.active && (
          <div className="form-actions" style={{ marginTop: 4 }}>
            <button className="btn" onClick={() => g.setField("tools", [...list, { name: "", url: "", icon: "", description: "" }])}>{t("general.external_tools.add")}</button>
          </div>
        )}
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

export function General({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <LanguageCard />
      <DaemonStrip canAdmin={canAdmin} />
      <ServiceLinksCard canAdmin={canAdmin} />
      <ExternalToolsCard canAdmin={canAdmin} />
    </>
  );
}
