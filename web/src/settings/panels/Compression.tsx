// settings/panels/Compressor.tsx — Sprint 12 (was H) Phase 5, split into two
// cards (Phase 7, 2026-08-13): Routing and Compressor are two halves of one
// request path, so this file's cards now render from Routing.tsx instead of
// their own standalone Settings section — see sections.tsx/Settings.tsx for
// the section merge. Compression proxy management: global + per-proxy
// passthrough toggle, restart/remove, tokens-saved display, add-proxy form.
import { useState } from "react";
import { ConfirmButton } from "../../components/ConfirmButton";
import { apiErrorMessage } from "../../lib/api";
import { formatTokens } from "../../lib/format";
import {
  useCompressorConfig,
  useCompressorCreateProxy,
  useCompressorPassthrough,
  useCompressorRestart,
  useCompressorTeardown,
  useProviders,
  useUpdateProvider,
  useUsage,
} from "../../lib/queries";

const SERVICE_RE = /^[a-z][a-z0-9_-]+$/;

export function CompressorModeCard({ canOperate }: { canOperate: boolean }) {
  const compressor = useCompressorConfig();
  const passthrough = useCompressorPassthrough();

  if (compressor.isError) {
    return (
      <>
        <div className="eyebrow" id="compressor-mode">Compressor · routing mode</div>
        <div className="card empty-note">Operator role required to view Compressor configuration.</div>
      </>
    );
  }

  const globalOn = compressor.data?.passthrough_all ?? false;

  return (
    <>
      <div className="eyebrow" id="compressor-mode">Compressor · routing mode</div>
      <div className="card" style={{ display: "flex", alignItems: "center", gap: 16, flexWrap: "wrap" }}>
        <span
          className={`toggle ${canOperate ? "" : "disabled"}`}
          title={canOperate ? "Bypass Compressor compression for every proxy — flips live, no teardown" : "Operator role required"}
          onClick={() => canOperate && !passthrough.isPending && passthrough.mutate({ scope: "all", enabled: !globalOn })}
        >
          <span className={`sw heat ${globalOn ? "on" : ""}`} />
          <span>
            Global passthrough <b style={{ color: "var(--text)" }}>{globalOn ? "on" : "off"}</b>
          </span>
        </span>
        <div style={{ fontSize: 12, color: "var(--text-mute)", maxWidth: 520, lineHeight: 1.5 }}>
          When on, every consumer routes straight to upstream and Compressor compression is bypassed for all proxies
          live — no teardown. Use to A/B compression quality or when debugging a provider.
        </div>
      </div>
    </>
  );
}

export function CompressorProxiesCard({ canOperate, canAdmin }: { canOperate: boolean; canAdmin: boolean }) {
  const compressor = useCompressorConfig();
  const usage = useUsage("7d");
  const restart = useCompressorRestart();
  const teardown = useCompressorTeardown();
  const passthrough = useCompressorPassthrough();
  const providers = useProviders();
  const updateProvider = useUpdateProvider();
  const createProxy = useCompressorCreateProxy();

  const [adding, setAdding] = useState(false);
  const [addMode, setAddMode] = useState<"provider" | "standalone">("provider");
  const [selectedProviderId, setSelectedProviderId] = useState<number | null>(null);
  const [serviceName, setServiceName] = useState("");
  const [label, setLabel] = useState("");
  const [targetUrl, setTargetUrl] = useState("");
  const [error, setError] = useState<string | null>(null);

  if (compressor.isError) {
    return null; // CompressorModeCard already renders the operator-role empty state above this.
  }

  const savingsByProxy = new Map((usage.data?.compressor ?? []).map((r) => [r.proxy, r]));
  const globalOn = compressor.data?.passthrough_all ?? false;

  const linkedProxyByProviderName = new Map(
    (compressor.data?.providers ?? []).map((p) => [p.name, p.compressor_proxy]),
  );
  const existingServices = new Set((compressor.data?.proxies ?? []).map((p) => p.service));
  const providerList = providers.data?.providers ?? [];
  const unlinkedProviders = providerList.filter(
    (p) => !linkedProxyByProviderName.get(p.name),
  );

  const serviceValid = SERVICE_RE.test(serviceName) && serviceName.length <= 64;
  const serviceTaken = existingServices.has(serviceName);

  function resetForm() {
    setAdding(false);
    setAddMode("provider");
    setSelectedProviderId(null);
    setServiceName("");
    setLabel("");
    setTargetUrl("");
    setError(null);
  }

  function onProviderSelect(id: number) {
    setSelectedProviderId(id);
    const p = providerList.find((p) => p.id === id);
    if (p) {
      const suggested = p.name.toLowerCase().replace(/[^a-z0-9_-]/g, "").replace(/^[^a-z]+/, "");
      setServiceName(suggested);
      setLabel(p.name);
      setTargetUrl(p.target_url ?? "");
    }
  }

  function submitCreate() {
    setError(null);
    if (addMode === "provider") {
      if (!selectedProviderId || !serviceValid || serviceTaken) return;
      updateProvider.mutate(
        { id: selectedProviderId, req: { compressor_proxy: serviceName } },
        {
          onSuccess: () => resetForm(),
          onError: (e) => setError(apiErrorMessage(e)),
        },
      );
    } else {
      if (!serviceValid || serviceTaken || !label || !targetUrl) return;
      createProxy.mutate(
        { service: serviceName, label, target_url: targetUrl },
        {
          onSuccess: () => resetForm(),
          onError: (e) => setError(apiErrorMessage(e)),
        },
      );
    }
  }

  const creating = updateProvider.isPending || createProxy.isPending;
  const canSubmit =
    serviceValid &&
    !serviceTaken &&
    (addMode === "provider"
      ? selectedProviderId != null
      : label.trim() !== "" && targetUrl.trim() !== "");

  return (
    <>
      <div className="eyebrow" id="compressor-proxies">Compressor · compression proxies</div>
      <div className="card">
        {!compressor.data && <div className="empty-note">Loading proxies…</div>}
        {canAdmin && (
          adding ? (
            <div className="form-grid" style={{ marginBottom: 14 }}>
              <label className="form-row" style={{ gridColumn: "1 / -1" }}>Mode
                <select
                  value={addMode}
                  onChange={(e) => {
                    setAddMode(e.target.value as "provider" | "standalone");
                    setError(null);
                  }}
                >
                  <option value="provider">Link to provider</option>
                  <option value="standalone">Standalone (no provider)</option>
                </select>
              </label>
              {addMode === "provider" && (
                <label className="form-row" style={{ gridColumn: "1 / -1" }}>Provider
                  {unlinkedProviders.length === 0 ? (
                    <span style={{ fontSize: 12, color: "var(--text-dim)" }}>
                      All providers already have a proxy linked. Create a standalone proxy instead, or add a new provider first.
                    </span>
                  ) : (
                    <select
                      value={selectedProviderId ?? ""}
                      onChange={(e) => onProviderSelect(Number(e.target.value))}
                    >
                      <option value="">Select a provider…</option>
                      {unlinkedProviders.map((p) => (
                        <option key={p.id} value={p.id}>{p.name}</option>
                      ))}
                    </select>
                  )}
                </label>
              )}
              {addMode === "standalone" && (
                <label className="form-row" style={{ gridColumn: "1 / -1" }}>Label
                  <input
                    value={label}
                    placeholder="Local upstream"
                    onChange={(e) => setLabel(e.target.value)}
                  />
                </label>
              )}
              <label className="form-row">Service name
                <input
                  value={serviceName}
                  placeholder="deepseek"
                  onChange={(e) => setServiceName(e.target.value)}
                  style={{ fontFamily: "var(--mono)" }}
                />
                <span style={{ fontSize: 11, color: serviceValid ? "var(--text-dim)" : "var(--warn)" }}>
                  {serviceTaken
                    ? "A proxy with this service name already exists."
                    : serviceValid
                      ? `Becomes the systemd unit forge-compress@${serviceName}.service`
                      : "Must match ^[a-z][a-z0-9_-]+$ — lowercase, starts with a letter."}
                </span>
              </label>
              {addMode === "standalone" && (
                <label className="form-row" style={{ gridColumn: "1 / -1" }}>Target URL
                  <input
                    value={targetUrl}
                    placeholder="https://api.example.com/v1"
                    onChange={(e) => setTargetUrl(e.target.value)}
                  />
                </label>
              )}
              {error && <div className="error-note" style={{ gridColumn: "1 / -1" }}>{error}</div>}
              <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn" onClick={resetForm}>Cancel</button>
                <button
                  className="btn primary"
                  disabled={creating || !canSubmit}
                  onClick={submitCreate}
                >
                  {creating ? "Creating…" : "Create proxy"}
                </button>
              </div>
            </div>
          ) : (
            <button className="btn" style={{ marginBottom: 14 }} onClick={() => { setAdding(true); setError(null); }}>
              + Add proxy
            </button>
          )
        )}
        <div className="hoom">
          {compressor.data?.proxies.map((p) => {
            const compressionOn = !p.passthrough;
            const savings = savingsByProxy.get(p.service);
            return (
              <div className="prox" key={p.service}>
                <span
                  className={`sw ${compressionOn ? "on" : ""}`}
                  style={{ flex: "0 0 auto", cursor: canOperate && !globalOn ? "pointer" : "default", opacity: globalOn ? 0.5 : 1 }}
                  title={
                    globalOn
                      ? "Overridden by global passthrough"
                      : compressionOn
                        ? "Compression on — click to bypass this proxy"
                        : "Bypassed — click to re-enable compression"
                  }
                  onClick={() =>
                    canOperate &&
                    !globalOn &&
                    !passthrough.isPending &&
                    passthrough.mutate({ scope: "proxy", service: p.service, enabled: compressionOn })
                  }
                />
                <div>
                  <div className="pn">
                    {p.label} <span className={`sd ${p.active ? "on" : "off"}`} style={{ display: "inline-block", marginLeft: 4 }} />
                  </div>
                  <div className="pu">
                    {p.unit} · :{p.port}
                    {p.orphaned ? ` · orphaned (${p.orphaned_days_left}d left)` : ""}
                  </div>
                </div>
                <div className="saved">
                  <span className="k">{p.passthrough ? "bypassed" : "compressed"} · tokens saved 7d</span>
                  {savings ? formatTokens(savings.saved) : "—"}
                </div>
                {canOperate && (
                  <div className="actions">
                    {/* Sprint K bug fix: restart.isPending used to disable
                        EVERY proxy's Restart button at once with no
                        indication which one was actually restarting
                        (restart is one mutation shared across the whole
                        list). Scoped to this proxy via restart.variables,
                        the same per-item disambiguation pattern
                        discoverBilling already uses in ProviderKeys. */}
                    {(() => {
                      const restartingThis = restart.isPending && restart.variables === p.service;
                      return (
                        <button disabled={restart.isPending} onClick={() => restart.mutate(p.service)}>
                          <span className={restartingThis ? "spin-icon" : ""} aria-hidden="true">↻</span>{" "}
                          {restartingThis ? "Restarting…" : "Restart"}
                        </button>
                      );
                    })()}
                    {canAdmin && (
                      <ConfirmButton
                        className=""
                        pending={teardown.isPending && teardown.variables === p.service}
                        onConfirm={() => teardown.mutate(p.service)}
                        label="Remove"
                        pendingLabel="Removing…"
                        warning={`Remove proxy "${p.label}"? This tears down the systemd unit and orphans the proxy row.`}
                      />
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 12, lineHeight: 1.5 }}>
          Tokens-saved figures are Compressor's own durable lifetime counters, scraped from each proxy's{" "}
          <span style={{ fontFamily: "var(--mono)" }}>/metrics</span> — Compressor-reported, not independently
          verified.
        </div>
      </div>
    </>
  );
}
