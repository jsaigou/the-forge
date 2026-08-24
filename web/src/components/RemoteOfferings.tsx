// components/RemoteOfferings.tsx — read-only display of store.Offering rows
// (MODEL CATALOG Phase 2). Lives on the Models page so operators can see
// what remote offerings exist WITHOUT Settings access (Console UX path A).
//
// Multi-provider sprint (2026-08-06): offerings of the same catalog model
// are ONE entry — the router presents exactly one model per group (the
// PRIMARY: enabled offering of an enabled provider with the lowest
// priority value, ties by provider name then id — mirrors offeringChain /
// BuildModelsResponse), so the same model on two providers no longer shows
// up twice (the glm-5.2-on-aiand-and-qwen case). Alternatives collapse
// into an expandable detail; the wire names still route via a0 as aliases
// converging on the primary.

import { countryFlag } from "../lib/format";
import { presetFor, providerIconSlug } from "../lib/providerPresets";
import { useCatalogModels, useCatalogOfferings, useProviders } from "../lib/queries";
import { Icon } from "./Icon";
import type { CatalogOffering, Provider } from "../lib/types";

export function RemoteOfferings() {
  const offerings = useCatalogOfferings();
  const models = useCatalogModels();
  const providers = useProviders();
  if (!offerings.data || !models.data) return null;

  const enabled = offerings.data.filter((o) => o.enabled);
  if (enabled.length === 0) return null;

  const providerList = providers.data?.providers ?? [];
  const providerByName = (name: string) => providerList.find((p) => p.name === name);
  const providerEnabled = (name: string) => providerByName(name)?.enabled ?? true;

  // Group by catalog model; within a group, order exactly like the router
  // (priority, provider name, id) so the first routable row IS the primary
  // a0 presents and serves.
  const groups = new Map<number, CatalogOffering[]>();
  for (const o of enabled) {
    const g = groups.get(o.model_id);
    if (g) g.push(o);
    else groups.set(o.model_id, [o]);
  }
  const orderedGroups = [...groups.values()].map((g) =>
    [...g].sort((a, b) => a.priority - b.priority || a.provider.localeCompare(b.provider) || a.id - b.id),
  );

  return (
    <div>
      <h2 style={{ fontSize: 13, fontWeight: 600, margin: "22px 0 10px" }}>Remote offerings</h2>
      <div className="hoom">
        {orderedGroups.map((group) => {
          const routable = group.filter((o) => providerEnabled(o.provider));
          const primary = routable[0] ?? group[0];
          const alternatives = group.filter((o) => o.id !== primary.id);
          const m = models.data!.find((m) => m.id === primary.model_id);
          return (
            <RemoteModelRow
              key={primary.model_id}
              primary={primary}
              alternatives={alternatives}
              modelName={m?.name ?? null}
              modelLogo={m?.logo ?? ""}
              modelLogoDark={m?.logo_dark ?? ""}
              providerByName={providerByName}
            />
          );
        })}
      </div>
    </div>
  );
}

function RemoteModelRow({
  primary,
  alternatives,
  modelName,
  modelLogo,
  modelLogoDark,
  providerByName,
}: {
  primary: CatalogOffering;
  alternatives: CatalogOffering[];
  modelName: string | null;
  modelLogo: string;
  modelLogoDark: string;
  providerByName: (name: string) => Provider | undefined;
}) {
  // All providers render (no "+N others" collapse — operator feedback
  // 2026-08-14). Router order is preserved by the parent's sort; the primary
  // (the offering a0 actually serves) is marked "preferred".
  const group = [primary, ...alternatives];
  return (
    <div className="ro-group">
      {(modelLogo || modelName) && (
        <div className="ro-head">
          {modelLogo ? <Icon slug={modelLogo} slugDark={modelLogoDark} name={modelName ?? ""} /> : <span className="icon" />}
          <span className="ro-model-name">{modelName ?? primary.wire_model}</span>
        </div>
      )}
      {group.map((o) => {
        const row = providerByName(o.provider);
        const disabled = row !== undefined && !row.enabled;
        const preferred = o.id === primary.id;
        return (
          <div key={o.id} className="ro-provider" style={{ opacity: disabled ? 0.6 : undefined }}>
            <Icon slug={providerIconSlug(o.provider)} name={o.provider} />
            <div className="ro-lines">
              {/* The model name lives in the group head — repeating it on
                  every provider row was the whitespace the operator flagged. */}
              <div className="ro-line1">
                <span>{o.wire_model}</span>
              </div>
              <div className="ro-line2">
                <span>{o.provider}</span>
                <ResidencyChip provider={row} fallbackName={o.provider} />
                {preferred && (
                  <span className="chip" style={{ color: "var(--ok)" }} title="The offering a0 currently presents for this model">
                    preferred
                  </span>
                )}
                {disabled && <span className="chip" style={{ color: "var(--warn)" }}>disabled — not routing</span>}
              </div>
              <div className="ro-line3">
                <span>{o.price_in_per_1m} {o.currency}/M in · {o.price_out_per_1m} out</span>
                {o.context_length > 0 && <span>· {o.context_length.toLocaleString()} ctx</span>}
                <span className="chip" style={{ color: "var(--text-mute)" }} title="Relative preference — lower is served first">priority {o.priority}</span>
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}

// ResidencyChip shows the provider-level residency fact. Source of truth is
// the provider row (country/data_residency_group, exposed since the
// multi-provider sprint); the curated preset is a fallback for rows created
// before those columns were surfaced.
function ResidencyChip({ provider, fallbackName }: { provider?: Provider; fallbackName: string }) {
  const preset = presetFor(fallbackName);
  const country = provider?.country || preset?.country || "";
  const group = provider?.data_residency_group || preset?.dataResidencyGroup || "";
  if (!country && !group) return null;
  return (
    <span className="chip" style={{ marginLeft: 6, color: "var(--text-dim)" }}>
      {country && countryFlag(country)} {country}{country && group ? " · " : ""}{group}
    </span>
  );
}
