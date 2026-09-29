import { useState } from "react";
import { useTranslation } from "react-i18next";
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
  // Peak pricing sprint (2026-09-12): whether this provider has a peak
  // schedule configured at all — drives OfferingForm's "no peak window
  // configured" hint, since a peak price set without one never applies.
  hasPeakWindows: boolean;
  // Server-computed "peak in force right now" — never re-derived from the
  // raw schedule client-side.
  peakActiveNow: boolean;
}

export function useCatalogProviders(): CatalogProviderRef[] {
  const providers = useProviders();
  return (providers.data?.providers ?? []).map((p) => ({
    name: p.name,
    country: p.country ?? "",
    data_residency_group: p.data_residency_group ?? "",
    enabled: p.enabled,
    hasPeakWindows: !!p.peak_windows,
    peakActiveNow: p.peak_active_now,
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
  const { t } = useTranslation("common");
  const [modelId, setModelId] = useState(existing?.model_id ?? 0);
  const [variantId, setVariantId] = useState(existing?.variant_id ?? 0);
  const [provider, setProvider] = useState(existing?.provider ?? "");
  const [wireModel, setWireModel] = useState(existing?.wire_model ?? "");
  const [priceIn, setPriceIn] = useState(existing?.price_in_per_1m ?? 0);
  const [priceOut, setPriceOut] = useState(existing?.price_out_per_1m ?? 0);
  const [priceCachedIn, setPriceCachedIn] = useState(existing?.price_cached_in_per_1m ?? null);
  const [priceInPeak, setPriceInPeak] = useState(existing?.price_in_per_1m_peak ?? null);
  const [priceOutPeak, setPriceOutPeak] = useState(existing?.price_out_per_1m_peak ?? null);
  const [priceCachedInPeak, setPriceCachedInPeak] = useState(existing?.price_cached_in_per_1m_peak ?? null);
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
        price_in_per_1m_peak: priceInPeak,
        price_out_per_1m_peak: priceOutPeak,
        price_cached_in_per_1m_peak: priceCachedInPeak,
        currency,
        context_length: contextLength,
        enabled,
        priority,
      },
      existing?.id,
    );
  }

  function fillPeakDouble() {
    setPriceInPeak(priceIn * 2);
    setPriceOutPeak(priceOut * 2);
    if (priceCachedIn != null) setPriceCachedInPeak(priceCachedIn * 2);
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">{t("catalog_form.offering.model_label")}
        <select value={modelId} onChange={(e) => { setModelId(Number(e.target.value)); setVariantId(0); }}>
          <option value={0}>{t("catalog_form.offering.select_ellipsis")}</option>
          {models.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
        </select>
      </label>
      <label className="form-row">{t("catalog_form.offering.variant_label")}
        <select value={variantId} onChange={(e) => setVariantId(Number(e.target.value))}>
          <option value={0}>{t("catalog_form.offering.none_option")}</option>
          {modelVariants.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
        </select>
      </label>
      <label className="form-row">{t("catalog_form.offering.provider_label")}
        <select value={provider} onChange={(e) => setProvider(e.target.value)}>
          <option value="">{t("catalog_form.offering.select_ellipsis")}</option>
          {providers.map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}{p.country ? ` ${countryFlag(p.country)}` : ""}{p.data_residency_group ? ` (${p.data_residency_group})` : ""}
            </option>
          ))}
        </select>
      </label>
      <label className="form-row">{t("catalog_form.offering.wire_model_label")}
        <input value={wireModel} placeholder={t("catalog_form.offering.wire_model_placeholder")} onChange={(e) => setWireModel(e.target.value)} />
      </label>
      <label className="form-row">{t("catalog_form.offering.price_in_label")}
        <input type="number" step="0.01" min={0} value={priceIn} onChange={(e) => setPriceIn(Number(e.target.value))} />
      </label>
      <label className="form-row">{t("catalog_form.offering.price_out_label")}
        <input type="number" step="0.01" min={0} value={priceOut} onChange={(e) => setPriceOut(Number(e.target.value))} />
      </label>
      <label className="form-row">{t("catalog_form.offering.cached_price_in_label")}
        <input
          type="number"
          step="0.001"
          min={0}
          value={priceCachedIn ?? ""}
          placeholder={t("catalog_form.offering.cached_price_placeholder")}
          onChange={(e) => setPriceCachedIn(e.target.value === "" ? null : Number(e.target.value))}
        />
      </label>
      <div style={{ gridColumn: "1 / -1", marginTop: 4 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 4 }}>
          <span style={{ fontSize: 12, fontWeight: 600 }}>{t("catalog_form.offering.peak_pricing_title")}</span>
          <button type="button" className="btn" style={{ fontSize: 11, padding: "2px 8px" }} onClick={fillPeakDouble}>
            {t("catalog_form.offering.fill_peak_button")}
          </button>
        </div>
        {selectedProvider && !selectedProvider.hasPeakWindows && (priceInPeak != null || priceOutPeak != null || priceCachedInPeak != null) && (
          <div style={{ fontSize: 11, color: "var(--warn)", marginBottom: 4 }}>
            {t("catalog_form.offering.no_peak_schedule_warning", { provider: selectedProvider.name })}
          </div>
        )}
        {((priceInPeak != null && priceInPeak < priceIn) || (priceOutPeak != null && priceOutPeak < priceOut)) && (
          <div style={{ fontSize: 11, color: "var(--warn)", marginBottom: 4 }}>
            {t("catalog_form.offering.peak_lower_warning")}
          </div>
        )}
      </div>
      <label className="form-row">{t("catalog_form.offering.price_in_peak_label")}
        <input
          type="number" step="0.01" min={0}
          value={priceInPeak ?? ""}
          placeholder={t("catalog_form.offering.same_as_off_peak_placeholder")}
          onChange={(e) => setPriceInPeak(e.target.value === "" ? null : Number(e.target.value))}
        />
      </label>
      <label className="form-row">{t("catalog_form.offering.price_out_peak_label")}
        <input
          type="number" step="0.01" min={0}
          value={priceOutPeak ?? ""}
          placeholder={t("catalog_form.offering.same_as_off_peak_placeholder")}
          onChange={(e) => setPriceOutPeak(e.target.value === "" ? null : Number(e.target.value))}
        />
      </label>
      <label className="form-row">{t("catalog_form.offering.cached_price_peak_label")}
        <input
          type="number" step="0.001" min={0}
          value={priceCachedInPeak ?? ""}
          placeholder={t("catalog_form.offering.same_as_off_peak_placeholder")}
          onChange={(e) => setPriceCachedInPeak(e.target.value === "" ? null : Number(e.target.value))}
        />
      </label>
      <label className="form-row">{t("catalog_form.offering.currency_label")}
        <input value={currency} maxLength={3} placeholder="USD" onChange={(e) => setCurrency(e.target.value.toUpperCase())} />
      </label>
      <label className="form-row">{t("catalog_form.offering.context_length_label")}
        <input type="number" min={0} value={contextLength} placeholder="65536" onChange={(e) => setContextLength(Number(e.target.value))} />
      </label>
      <label className="form-row">{t("catalog_form.offering.priority_label")}
        <input type="number" min={0} value={priority} onChange={(e) => setPriority(Number(e.target.value))} />
        <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
          {t("catalog_form.offering.priority_hint")}
        </span>
      </label>
      <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        {t("catalog_form.offering.enabled_checkbox")}
      </label>
      {selectedProvider?.country && (
        <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)" }}>
          {countryFlag(selectedProvider.country)} {t("catalog_form.offering.data_residency_label", { country: selectedProvider.country })}
          {selectedProvider.data_residency_group ? t("catalog_form.offering.data_residency_group_suffix", { group: selectedProvider.data_residency_group }) : ""}
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>{t("catalog_form.offering.cancel")}</button>
        <SaveButton
          pending={pending}
          isError={isError}
          disabled={pending || !modelId || !provider || !wireModel}
          onClick={submit}
          label={existing ? undefined : t("save_button.create")}
        />
      </div>
    </div>
  );
}
