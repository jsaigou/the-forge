// components/RemoteOfferings.tsx — display of store.Offering rows (MODEL
// CATALOG Phase 2). Lives on the Models page so operators can see what
// remote offerings exist WITHOUT Settings access (Console UX path A).
//
// Multi-provider sprint (2026-08-06): offerings of the same catalog model
// are ONE entry — the router presents exactly one model per group (the
// PRIMARY: enabled offering of an enabled provider with the lowest
// priority value, ties by provider name then id — mirrors offeringChain /
// BuildModelsResponse), so the same model on two providers no longer shows
// up twice (the glm-5.2-on-aiand-and-qwen case). Alternatives collapse
// into an expandable detail; the wire names still route via a0 as aliases
// converging on the primary.
//
// Offerings CRUD sprint (2026-09-06): this page was read-only — no way to
// add an offering or change which model/variant one serves — forcing every
// edit through Settings → Catalog. Admins now get full CRUD here, sharing
// the exact form Settings → Catalog → Offerings uses
// (components/catalog/OfferingForm.tsx), plus visibility into disabled
// offerings (otherwise a freshly-created-but-disabled row would vanish
// with no way back). Non-admin viewers still see the original read-only,
// enabled-only view.
import { useState } from "react";
import { OfferingForm, useCatalogProviders } from "./catalog/OfferingForm";
import { countryFlag } from "../lib/format";
import { groupOfferingsByModel, preferredOfferingIds } from "../lib/offeringPreference";
import { presetFor, providerIconSlug } from "../lib/providerPresets";
import {
  useCatalogModels,
  useCatalogOfferings,
  useCatalogVariants,
  useCreateCatalogOffering,
  useDeleteCatalogOffering,
  useProviders,
  useUpdateCatalogOffering,
} from "../lib/queries";
import { useSession } from "../lib/session";
import { useErrorState } from "../lib/useErrorState";
import { ConfirmButton } from "./ConfirmButton";
import { Icon } from "./Icon";
import type { CatalogOffering, Provider } from "../lib/types";

export function RemoteOfferings() {
  const { canAdmin } = useSession();
  const offerings = useCatalogOfferings();
  const models = useCatalogModels();
  const variants = useCatalogVariants();
  const providers = useProviders();
  const formProviders = useCatalogProviders();
  const create = useCreateCatalogOffering();
  const update = useUpdateCatalogOffering();
  const remove = useDeleteCatalogOffering();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  if (!offerings.data || !models.data) return null;

  const all = offerings.data;
  const visible = canAdmin ? all : all.filter((o) => o.enabled);
  if (!canAdmin && visible.length === 0) return null;

  const modelList = models.data;
  const variantList = variants.data ?? [];
  const providerList = providers.data?.providers ?? [];
  const providerByName = (name: string) => providerList.find((p) => p.name === name);
  const providerEnabled = (name: string) => providerByName(name)?.enabled ?? true;

  // Same primary-selection rule as the router (offeringPreference.ts,
  // mirrors router.SelectOfferingChain) — shared so this badge can never
  // drift from Settings → Routing's, and a disabled row can never win it.
  const preferredIds = preferredOfferingIds(all, providerEnabled);
  const groups = [...groupOfferingsByModel(visible).values()].map((group) => {
    const routable = group.filter((o) => o.enabled && providerEnabled(o.provider));
    const anchor = routable[0] ?? group[0];
    return [anchor, ...group.filter((o) => o.id !== anchor.id)];
  });

  const editingOffering = editing === "new" ? null : all.find((o) => o.id === editing);

  function handleSubmit(draft: Partial<CatalogOffering>, id?: number) {
    clearError();
    // Delayed close so SaveButton's success flash has time to paint.
    if (id) {
      update.mutate({ id, o: draft }, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    } else {
      create.mutate(draft, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    }
  }

  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  // UpdateOffering is a full replace at the store layer, not a merge — the
  // handler writes every column straight from the request body, so a quick
  // toggle must resend the whole record (mirrors OfferingForm's submit())
  // rather than a bare {enabled} patch, or it would silently blank the rest
  // of the row (the exact "Full-replace curl verification hazard" from the
  // 2026-08-05 incident, but reachable from this button too if skipped).
  function handleToggleEnabled(o: CatalogOffering) {
    clearError();
    update.mutate({
      id: o.id,
      o: {
        model_id: o.model_id,
        variant_id: o.variant_id,
        provider: o.provider,
        wire_model: o.wire_model,
        price_in_per_1m: o.price_in_per_1m,
        price_out_per_1m: o.price_out_per_1m,
        price_cached_in_per_1m: o.price_cached_in_per_1m,
        currency: o.currency,
        context_length: o.context_length,
        enabled: !o.enabled,
        priority: o.priority,
      },
    }, { onError: showError });
  }

  return (
    <div>
      <h2 style={{ fontSize: 13, fontWeight: 600, margin: "22px 0 10px" }}>Remote offerings</h2>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {editing !== null ? (
        <OfferingForm
          key={editingOffering?.id ?? "new"}
          existing={editingOffering ?? undefined}
          models={modelList}
          variants={variantList}
          providers={formProviders}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          <div className="hoom">
            {groups.map((group) => {
              const m = modelList.find((m) => m.id === group[0].model_id);
              return (
                <RemoteModelRow
                  key={group[0].model_id}
                  group={group}
                  preferredIds={preferredIds}
                  modelName={m?.name ?? null}
                  modelLogo={m?.logo ?? ""}
                  modelLogoDark={m?.logo_dark ?? ""}
                  providerByName={providerByName}
                  canAdmin={canAdmin}
                  deletePending={remove.isPending}
                  togglePending={update.isPending}
                  onEdit={(id) => { setEditing(id); clearError(); }}
                  onDelete={handleDelete}
                  onToggleEnabled={handleToggleEnabled}
                />
              );
            })}
          </div>
          {visible.length === 0 && <div className="empty-note">No offerings. Create one to route to a remote provider.</div>}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              + New offering
            </button>
          )}
        </>
      )}
    </div>
  );
}

function RemoteModelRow({
  group,
  preferredIds,
  modelName,
  modelLogo,
  modelLogoDark,
  providerByName,
  canAdmin,
  deletePending,
  togglePending,
  onEdit,
  onDelete,
  onToggleEnabled,
}: {
  group: CatalogOffering[];
  preferredIds: Set<number>;
  modelName: string | null;
  modelLogo: string;
  modelLogoDark: string;
  providerByName: (name: string) => Provider | undefined;
  canAdmin: boolean;
  deletePending: boolean;
  togglePending: boolean;
  onEdit: (id: number) => void;
  onDelete: (id: number) => void;
  onToggleEnabled: (o: CatalogOffering) => void;
}) {
  // All providers render (no "+N others" collapse — operator feedback
  // 2026-08-14). The anchor (the offering a0 actually serves, or the
  // group's own head when nothing is routable) leads; the rest follow in
  // router-priority order.
  return (
    <div className="ro-group">
      {(modelLogo || modelName) && (
        <div className="ro-head">
          {modelLogo ? <Icon slug={modelLogo} slugDark={modelLogoDark} name={modelName ?? ""} /> : <span className="icon" />}
          <span className="ro-model-name">{modelName ?? group[0].wire_model}</span>
        </div>
      )}
      {group.map((o) => {
        const row = providerByName(o.provider);
        const providerDisabled = row !== undefined && !row.enabled;
        const preferred = preferredIds.has(o.id);
        return (
          <div key={o.id} className="ro-provider" style={{ opacity: providerDisabled || !o.enabled ? 0.6 : undefined }}>
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
                {!o.enabled && <span className="chip" style={{ color: "var(--warn)" }}>disabled</span>}
                {o.enabled && providerDisabled && <span className="chip" style={{ color: "var(--warn)" }}>disabled — not routing</span>}
              </div>
              <div className="ro-line3">
                <span>{o.price_in_per_1m} {o.currency}/M in · {o.price_out_per_1m} out</span>
                {o.context_length > 0 && <span>· {o.context_length.toLocaleString()} ctx</span>}
                <span className="chip" style={{ color: "var(--text-mute)" }} title="Relative preference — lower is served first">priority {o.priority}</span>
              </div>
            </div>
            {canAdmin && (
              <div className="actions" style={{ display: "flex", gap: 6 }}>
                <button
                  className="btn"
                  style={{ fontSize: 11, padding: "4px 8px" }}
                  disabled={togglePending}
                  onClick={() => onToggleEnabled(o)}
                >
                  {o.enabled ? "Disable" : "Enable"}
                </button>
                <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => onEdit(o.id)}>Edit</button>
                <ConfirmButton
                  className="btn"
                  style={{ fontSize: 11, padding: "4px 8px" }}
                  pending={deletePending}
                  onConfirm={() => onDelete(o.id)}
                  warning={`Delete offering "${o.wire_model}"?`}
                />
              </div>
            )}
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
