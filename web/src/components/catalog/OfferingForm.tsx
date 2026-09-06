import { useState } from "react";
import { countryFlag } from "../../lib/format";
import { useProviders } from "../../lib/queries";
import type { CatalogModel, CatalogOffering, CatalogVariant } from "../../lib/types";
import { SaveButton } from "../SaveButton";

// OfferingForm — moved out of CatalogPanel.tsx (Models-page offerings CRUD
// sprint, 2026-09-06) so RemoteOfferings.tsx (Models → Offerings) can reuse
// the exact same create/edit form Settings → Catalog → Offerings already
// had, instead of duplicating it. Reused verbatim by both call sites.

// Provider list for the Offering form dropdown + the offerings table. Uses
// the /api/v1/providers endpoint (router_providers table). country /
// data_residency_group are Provider-level facts (ADR-0003), exposed by the
// endpoint since the multi-provider sprint (2026-08-06); enabled drives the
// "provider disabled" styling + the preferred-primary computation.
export interface CatalogProviderRef {
  name: string;
  country: string;
  data_residency_group: string;
  enabled: boolean;
}

export function useCatalogProviders(): CatalogProviderRef[] {
  const providers = useProviders();
  return (providers.data?.providers ?? []).map((p) => ({
    name: p.name,
    country: p.country ?? "",
    data_residency_group: p.data_residency_group ?? "",
    enabled: p.enabled,
  }));
}

export function OfferingForm({
  existing,
  models,
  variants,
  providers,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogOffering;
  models: CatalogModel[];
  variants: CatalogVariant[];
  providers: CatalogProviderRef[];
  onSubmit: (draft: Partial<CatalogOffering>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const [modelId, setModelId] = useState(existing?.model_id ?? 0);
  const [variantId, setVariantId] = useState(existing?.variant_id ?? 0);
  const [provider, setProvider] = useState(existing?.provider ?? "");
  const [wireModel, setWireModel] = useState(existing?.wire_model ?? "");
  const [priceIn, setPriceIn] = useState(existing?.price_in_per_1m ?? 0);
  const [priceOut, setPriceOut] = useState(existing?.price_out_per_1m ?? 0);
  const [priceCachedIn, setPriceCachedIn] = useState(existing?.price_cached_in_per_1m ?? null);
  const [currency, setCurrency] = useState(existing?.currency ?? "USD");
  const [contextLength, setContextLength] = useState(existing?.context_length ?? 0);
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [priority, setPriority] = useState(existing?.priority ?? 100);

  const modelVariants = variants.filter((v) => v.model_id === modelId);
  const selectedProvider = providers.find((p) => p.name === provider);

  function submit() {
    onSubmit(
      {
        model_id: modelId,
        variant_id: variantId,
        provider,
        wire_model: wireModel,
        price_in_per_1m: priceIn,
        price_out_per_1m: priceOut,
        price_cached_in_per_1m: priceCachedIn,
        currency,
        context_length: contextLength,
        enabled,
        priority,
      },
      existing?.id,
    );
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Model *
        <select value={modelId} onChange={(e) => { setModelId(Number(e.target.value)); setVariantId(0); }}>
          <option value={0}>— select —</option>
          {models.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
        </select>
      </label>
      <label className="form-row">Variant (optional)
        <select value={variantId} onChange={(e) => setVariantId(Number(e.target.value))}>
          <option value={0}>— none —</option>
          {modelVariants.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
        </select>
      </label>
      <label className="form-row">Provider *
        <select value={provider} onChange={(e) => setProvider(e.target.value)}>
          <option value="">— select —</option>
          {providers.map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}{p.country ? ` ${countryFlag(p.country)}` : ""}{p.data_residency_group ? ` (${p.data_residency_group})` : ""}
            </option>
          ))}
        </select>
      </label>
      <label className="form-row">Wire model *
        <input value={wireModel} placeholder="deepseek-chat" onChange={(e) => setWireModel(e.target.value)} />
      </label>
      <label className="form-row">Price in / 1M
        <input type="number" step="0.01" min={0} value={priceIn} onChange={(e) => setPriceIn(Number(e.target.value))} />
      </label>
      <label className="form-row">Price out / 1M
        <input type="number" step="0.01" min={0} value={priceOut} onChange={(e) => setPriceOut(Number(e.target.value))} />
      </label>
      <label className="form-row">Cached price in / 1M (optional)
        <input
          type="number"
          step="0.001"
          min={0}
          value={priceCachedIn ?? ""}
          placeholder="unmodelled — full price applies to cache hits"
          onChange={(e) => setPriceCachedIn(e.target.value === "" ? null : Number(e.target.value))}
        />
      </label>
      <label className="form-row">Currency
        <input value={currency} maxLength={3} placeholder="USD" onChange={(e) => setCurrency(e.target.value.toUpperCase())} />
      </label>
      <label className="form-row">Context length
        <input type="number" min={0} value={contextLength} placeholder="65536" onChange={(e) => setContextLength(Number(e.target.value))} />
      </label>
      <label className="form-row">Priority
        <input type="number" min={0} value={priority} onChange={(e) => setPriority(Number(e.target.value))} />
        <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
          When several providers offer this model, the LOWEST value is served via a0 (100 = no preference; ties break by provider name).
        </span>
      </label>
      <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        Enabled (route to this offering)
      </label>
      {selectedProvider?.country && (
        <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)" }}>
          {countryFlag(selectedProvider.country)} Data residency: {selectedProvider.country}
          {selectedProvider.data_residency_group ? ` · group: ${selectedProvider.data_residency_group}` : ""}
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton
          pending={pending}
          isError={isError}
          disabled={pending || !modelId || !provider || !wireModel}
          onClick={submit}
          label={existing ? "Save" : "Create"}
        />
      </div>
    </div>
  );
}
