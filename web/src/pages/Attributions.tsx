import type { ReactNode } from "react";
import { ICON_ATTRIBUTIONS, type IconSource } from "../assets/icons/attributions";
import { Icon } from "../components/Icon";
import { GO_DEPS, NPM_DEPS, RUNTIME_STACK } from "../lib/attributions.generated";
import { useCatalogModels, useStatus } from "../lib/queries";

// Sprint F — this app redistributes a lot of other people's work (31
// vendored icons across three sources, ~2 dozen Go modules, a handful of
// npm packages, the models themselves) with no credit surface anywhere.
// Every section here is either live data (models — the catalog *is* the
// source of truth) or generated from the same script that actually vendors
// the thing being credited (icons via vendor-icons.mjs, deps via
// gen-attributions.mjs), so a newly-added dependency can't go uncredited
// the way a hand-maintained list would. Only the external runtime stack
// (llama.cpp/headroom-ai/ComfyUI/vLLM + the always-on Qwen3 TTS/embedding
// and parakeet STT services) is curated — nothing in the repo declares
// those, since they're separately-installed processes, not code
// dependencies.

const ICON_SOURCE_LABEL: Record<IconSource, string> = {
  svgl: "pheralb/svgl",
  lobehub: "lobehub/lobe-icons",
  inline: "authored in-repo",
  raster: "brand mark (raster)",
};

const ICON_SOURCE_ORDER: IconSource[] = ["svgl", "lobehub", "raster", "inline"];

function LinkOrText({ href, children }: { href: string; children: ReactNode }) {
  if (!href) return <span>{children}</span>;
  return (
    <a href={href} target="_blank" rel="noreferrer noopener">
      {children}
    </a>
  );
}

export function Attributions() {
  const status = useStatus();
  const catalogModels = useCatalogModels();
  const models = [...(catalogModels.data ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  const directNpm = NPM_DEPS.filter((d) => d.direct);
  const toolingNpm = NPM_DEPS.filter((d) => !d.direct);
  const directGo = GO_DEPS.filter((d) => !d.indirect);
  const indirectGo = GO_DEPS.filter((d) => d.indirect);

  return (
    <section className="page">
      <div className="eyebrow">This project</div>
      <div className="card">
        <h3><span className="tick" />The Forge</h3>
        <div className="hoom">
          <div className="prox">
            <span className="pn">Version</span>
            <span className="pu">{status.data?.version || "unknown"}</span>
          </div>
          <div className="prox">
            <span className="pn">License</span>
            <span className="pu">
              Apache-2.0 — <LinkOrText href="https://github.com/jsaigou/the-forge/blob/main/LICENSE">LICENSE</LinkOrText>
            </span>
          </div>
        </div>
      </div>

      <div className="eyebrow">Models</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Every model in the live catalog and its own license, as recorded on the model — not a
          static list, so it stays correct as the catalog changes.
        </div>
        {models.length === 0 ? (
          <div className="empty-note">Loading catalog…</div>
        ) : (
          <div className="hoom">
            {models.map((m) => (
              <div className="prox" key={m.id}>
                {m.logo && <Icon slug={m.logo} name={m.name} sm />}
                <span className="pn">{m.name}</span>
                {m.creator && <span className="pu">{m.creator}</span>}
                <span className="pu" style={{ marginLeft: "auto" }}>
                  {m.license_name ? (
                    <LinkOrText href={m.license_url}>{m.license_name}</LinkOrText>
                  ) : (
                    <span style={{ color: "var(--text-mute)" }}>license not recorded</span>
                  )}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="eyebrow">Runtime stack</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Separately-installed services this app launches or proxies to — not code dependencies,
          so nothing in the repo declares them; this table is hand-maintained.
        </div>
        <div className="hoom">
          {RUNTIME_STACK.map((r) => (
            <div className="prox" key={r.name} style={{ flexDirection: "column", alignItems: "stretch", gap: 4 }}>
              <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
                <span className="pn">
                  <LinkOrText href={r.projectUrl}>{r.name}</LinkOrText>
                </span>
                <span className="chip">{r.license}</span>
              </div>
              <span className="pu">{r.role}</span>
            </div>
          ))}
        </div>
      </div>

      <div className="eyebrow">Software</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Generated from <code>go/go.mod</code> and <code>web/package.json</code> at build time —
          see <code>web/scripts/gen-attributions.mjs</code>.
        </div>
        <div style={{ fontSize: 11.5, fontWeight: 600, color: "var(--text-dim)", margin: "4px 0 8px" }}>Go — direct</div>
        <div className="hoom">
          {directGo.map((d) => (
            <div className="prox" key={d.path}>
              <span className="pn"><LinkOrText href={d.projectUrl}>{d.path}</LinkOrText></span>
              <span className="pu">{d.version}</span>
              <span className="chip" style={{ marginLeft: "auto" }}>{d.license}</span>
            </div>
          ))}
        </div>
        <div style={{ fontSize: 11.5, fontWeight: 600, color: "var(--text-dim)", margin: "16px 0 8px" }}>npm — direct</div>
        <div className="hoom">
          {directNpm.map((d) => (
            <div className="prox" key={d.name}>
              <span className="pn"><LinkOrText href={d.projectUrl}>{d.name}</LinkOrText></span>
              <span className="pu">{d.version}</span>
              <span className="chip" style={{ marginLeft: "auto" }}>{d.license}</span>
            </div>
          ))}
        </div>
        <details style={{ marginTop: 16 }}>
          <summary style={{ cursor: "pointer", fontSize: 11.5, fontWeight: 600, color: "var(--text-dim)" }}>
            Build tooling — {toolingNpm.length} npm, indirect Go — {indirectGo.length}
          </summary>
          <div style={{ fontSize: 11.5, fontWeight: 600, color: "var(--text-dim)", margin: "12px 0 8px" }}>npm — tooling</div>
          <div className="hoom">
            {toolingNpm.map((d) => (
              <div className="prox" key={d.name}>
                <span className="pn"><LinkOrText href={d.projectUrl}>{d.name}</LinkOrText></span>
                <span className="pu">{d.version}</span>
                <span className="chip" style={{ marginLeft: "auto" }}>{d.license}</span>
              </div>
            ))}
          </div>
          <div style={{ fontSize: 11.5, fontWeight: 600, color: "var(--text-dim)", margin: "16px 0 8px" }}>Go — indirect</div>
          <div className="hoom">
            {indirectGo.map((d) => (
              <div className="prox" key={d.path}>
                <span className="pn"><LinkOrText href={d.projectUrl}>{d.path}</LinkOrText></span>
                <span className="pu">{d.version}</span>
                <span className="chip" style={{ marginLeft: "auto" }}>{d.license}</span>
              </div>
            ))}
          </div>
        </details>
      </div>

      <div className="eyebrow">Icons</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Generated from <code>web/scripts/vendor-icons.mjs</code> — the same script that fetches
          and vendors every mark below.
        </div>
        {ICON_SOURCE_ORDER.map((source) => {
          const group = ICON_ATTRIBUTIONS.filter((a) => a.source === source);
          if (group.length === 0) return null;
          return (
            <div key={source} style={{ marginBottom: 16 }}>
              <div style={{ fontSize: 11.5, fontWeight: 600, color: "var(--text-dim)", margin: "4px 0 8px" }}>
                {ICON_SOURCE_LABEL[source]}
              </div>
              <div className="hoom">
                {group.map((a) => (
                  <div className="prox" key={a.slug}>
                    <Icon slug={a.slug} name={a.slug} sm />
                    <span className="pn">{a.slug}</span>
                    <span className="pu">{a.sourceFile}</span>
                    <span className="pu" style={{ marginLeft: "auto" }}>
                      <LinkOrText href={a.licenseUrl || a.projectUrl}>{a.license}</LinkOrText>
                    </span>
                  </div>
                ))}
              </div>
            </div>
          );
        })}
      </div>
    </section>
  );
}
