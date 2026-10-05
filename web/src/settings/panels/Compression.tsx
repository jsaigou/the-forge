// settings/panels/Compression.tsx — the compressor is RETIRED (CONTRACTS C7,
// ADR-0017 "context is append-only", ai-mode#46). Both cards still render from
// Routing.tsx, but neither offers any way to turn compression back on:
//   * CompressorModeCard — a "Retired" badge + one paragraph on why, and an
//     honest warning if the store's passthrough_all is somehow off.
//   * CompressorProxiesCard — a read-only list of leftover proxy rows with a
//     Remove (teardown) action so existing units can be cleaned up. No add
//     form, no per-proxy bypass switch, no restart.
// The old enable/bypass controls, add-proxy form and savings display were
// removed here; the underlying code is deleted later by WS-R2.
import { useTranslation } from "react-i18next";
import { ConfirmButton } from "../../components/ConfirmButton";
import { useCompressorConfig, useCompressorTeardown } from "../../lib/queries";

export function CompressorModeCard({ canOperate: _canOperate }: { canOperate: boolean }) {
  const { t } = useTranslation("settings");
  const compressor = useCompressorConfig();

  if (compressor.isError) {
    return (
      <>
        <div className="eyebrow" id="compressor-mode">{t("compression.mode_title")}</div>
        <div className="card empty-note">{t("compression.role_required")}</div>
      </>
    );
  }

  // Only warn once the config has loaded and says passthrough is genuinely
  // off; while loading, show nothing alarming.
  const bypassOff = compressor.data ? compressor.data.passthrough_all === false : false;

  return (
    <>
      <div className="eyebrow" id="compressor-mode">{t("compression.mode_title")}</div>
      <div className="card" data-testid="compressor-retired-card">
        <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", marginBottom: 8 }}>
          <span className="chip" style={{ fontWeight: 600 }}>{t("compression.retired.badge")}</span>
          <b>{t("compression.retired.title")}</b>
        </div>
        <div style={{ fontSize: 13, color: "var(--text-mute)", maxWidth: 680, lineHeight: 1.55 }}>
          {t("compression.retired.body")}
        </div>
        {bypassOff ? (
          <div className="error-note" style={{ marginTop: 10 }} role="alert">
            {t("compression.retired.warn_not_bypassed")}
          </div>
        ) : (
          <div style={{ fontSize: 12, marginTop: 10 }}>{t("compression.retired.status_bypassed")}</div>
        )}
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 6 }}>
          {t("compression.retired.no_controls")}
        </div>
      </div>
    </>
  );
}

export function CompressorProxiesCard({ canOperate: _canOperate, canAdmin }: { canOperate: boolean; canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const compressor = useCompressorConfig();
  const teardown = useCompressorTeardown();

  if (compressor.isError) {
    return null; // CompressorModeCard already renders the operator-role empty state above this.
  }

  const proxies = compressor.data?.proxies ?? [];

  return (
    <>
      <div className="eyebrow" id="compressor-proxies">{t("compression.proxies_title")}</div>
      <div className="card">
        {!compressor.data && <div className="empty-note">{t("compression.loading_proxies")}</div>}
        {compressor.data && proxies.length === 0 && (
          <div className="empty-note">{t("compression.retired.no_proxies")}</div>
        )}
        <div className="hoom">
          {proxies.map((p) => (
            <div className="prox" key={p.service}>
              <div>
                <div className="pn">
                  {p.label}{" "}
                  <span className="chip" style={{ marginLeft: 4 }}>{t("compression.retired.row_badge")}</span>
                </div>
                <div className="pu">
                  {p.unit} · :{p.port}
                  {p.orphaned ? t("compression.orphaned_suffix", { days: p.orphaned_days_left }) : ""}
                  {" · "}
                  {t("compression.retired.row_state")}
                </div>
              </div>
              {canAdmin && !p.orphaned && (
                <div className="actions">
                  <ConfirmButton
                    className=""
                    pending={teardown.isPending && teardown.variables === p.service}
                    onConfirm={() => teardown.mutate(p.service)}
                    label={t("compression.remove_button")}
                    pendingLabel={t("compression.removing")}
                    warning={t("compression.remove_warning", { label: p.label })}
                  />
                </div>
              )}
            </div>
          ))}
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 12, lineHeight: 1.5 }}>
          {t("compression.retired.proxies_note")}
        </div>
      </div>
    </>
  );
}
