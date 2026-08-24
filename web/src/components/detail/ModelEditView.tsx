import { useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import { familyInheritedIcon } from "../../lib/iconInheritance";
import {
  useCatalogFamilies,
  useCatalogGenealogies,
  useCatalogModels,
  useUpdateCatalogModel,
  useUploadModelIcon,
} from "../../lib/queries";
import type { CatalogFamily, CatalogGenealogy, CatalogModel } from "../../lib/types";
import { IconPicker } from "../IconPicker";
import { SaveButton } from "../SaveButton";

// ModelEditView (Sprint C) — model edit-in-place, pushed into DetailModal's
// view stack from ModelDetailView's Edit button. Replaces the old
// goToModelEdit cross-tab jump (App.tsx NavigationContext → Settings →
// Catalog → Models, consuming the model's id via initialEditId) — that
// whole mechanism is now dead and removed alongside this file landing.
// Same section styling as ConfigEditView for visual consistency across the
// two editors.
export function ModelEditView({ modelId, onDone, onCancel }: { modelId: string; onDone: () => void; onCancel: () => void }) {
  const models = useCatalogModels();
  const families = useCatalogFamilies();
  const genealogies = useCatalogGenealogies();
  const update = useUpdateCatalogModel();
  const uploadIcon = useUploadModelIcon();
  const [error, setError] = useState<string | null>(null);

  const existing = models.data?.find((m) => String(m.id) === modelId);
  if (!existing) {
    return <div className="empty-note">Loading model…</div>;
  }

  return (
    <ModelEditForm
      existing={existing}
      families={families.data ?? []}
      genealogies={genealogies.data ?? []}
      pending={update.isPending}
      error={error}
      onCancel={onCancel}
      onUploadIcon={(file, dark) => uploadIcon.mutate({ id: existing.id, file, dark }, { onError: (e) => setError(apiErrorMessage(e)) })}
      onSelectIcon={(slug, dark) => update.mutate({ id: existing.id, m: dark ? { ...existing, logo_dark: slug } : { ...existing, logo: slug } }, { onError: (e) => setError(apiErrorMessage(e)) })}
      onClearIcon={(dark) => update.mutate({ id: existing.id, m: dark ? { ...existing, logo_dark: "" } : { ...existing, logo: "" } }, { onError: (e) => setError(apiErrorMessage(e)) })}
      onSubmit={(draft, reason) => {
        setError(null);
        update.mutate(
          { id: existing.id, m: draft, reason },
          // Sprint K: delayed onDone so SaveButton's flash has time to
          // paint before this view pops off DetailModal's stack.
          { onSuccess: () => setTimeout(onDone, 700), onError: (e) => setError(apiErrorMessage(e)) },
        );
      }}
    />
  );
}

function ModelEditForm({
  existing,
  families,
  genealogies,
  onSubmit,
  onCancel,
  onUploadIcon,
  onSelectIcon,
  onClearIcon,
  pending,
  error,
}: {
  existing: CatalogModel;
  families: CatalogFamily[];
  genealogies: CatalogGenealogy[];
  onSubmit: (draft: Partial<CatalogModel>, reason: string) => void;
  onCancel: () => void;
  onUploadIcon: (file: File, dark?: boolean) => void;
  onSelectIcon: (slug: string, dark?: boolean) => void;
  onClearIcon: (dark?: boolean) => void;
  pending: boolean;
  error: string | null;
}) {
  const [reason, setReason] = useState("");
  const [familyId, setFamilyId] = useState(existing.family_id);
  const [name, setName] = useState(existing.name);
  const [architecture, setArchitecture] = useState(existing.architecture);
  const [parameterCount, setParameterCount] = useState(existing.parameter_count);
  const [description, setDescription] = useState(existing.description);
  const [creator, setCreator] = useState(existing.creator);
  const [licenseName, setLicenseName] = useState(existing.license_name);
  const [licenseUrl, setLicenseUrl] = useState(existing.license_url);
  const [hfRepo, setHfRepo] = useState(existing.hf_repo);
  const [keyFeaturesText, setKeyFeaturesText] = useState(existing.key_features.join("\n"));

  function submit() {
    onSubmit(
      {
        family_id: familyId,
        name,
        architecture,
        parameter_count: parameterCount,
        description,
        creator,
        license_name: licenseName,
        license_url: licenseUrl,
        hf_repo: hfRepo,
        key_features: keyFeaturesText.split("\n").map((s) => s.trim()).filter((s) => s.length > 0),
        // Carried through unchanged — UpdateModel is full-replace and icon
        // changes go through the immediate onSelectIcon/UploadIcon/ClearIcon
        // mutations below, not this form's own Save (Sprint I; this draft
        // never included logo pre-Sprint-I either, silently wiping the
        // model's icon on any other-field edit).
        logo: existing.logo,
        logo_dark: existing.logo_dark,
      },
      reason,
    );
  }

  return (
    <div className="model-edit-view">
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      <div className="eyebrow" style={{ marginTop: 0 }}>Identity</div>
      <div className="form-grid">
        <label className="form-row">Name *
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="form-row">Family
          <select value={familyId} onChange={(e) => setFamilyId(Number(e.target.value))}>
            <option value={0}>— none —</option>
            {families.map((f) => <option key={f.id} value={f.id}>{f.name}</option>)}
          </select>
        </label>
        <label className="form-row">Architecture
          <input value={architecture} placeholder="llama" onChange={(e) => setArchitecture(e.target.value)} />
        </label>
        <label className="form-row">Parameter count
          <input value={parameterCount} placeholder="31B" onChange={(e) => setParameterCount(e.target.value)} />
        </label>
        <label className="form-row">Creator
          <input value={creator} placeholder="Qwen" onChange={(e) => setCreator(e.target.value)} />
        </label>
        <label className="form-row">HF repo
          <input value={hfRepo} placeholder="Qwen/Qwen3-Coder-Next" onChange={(e) => setHfRepo(e.target.value)} />
        </label>
        <label className="form-row" style={{ gridColumn: "1 / -1" }}>Description
          <textarea
            value={description}
            rows={3}
            style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 13, resize: "vertical" }}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        <label className="form-row" style={{ gridColumn: "1 / -1" }}>Key features (one per line)
          <textarea
            value={keyFeaturesText}
            rows={3}
            style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 12, resize: "vertical" }}
            onChange={(e) => setKeyFeaturesText(e.target.value)}
          />
        </label>
      </div>

      <div className="eyebrow">License</div>
      <div className="form-grid">
        <label className="form-row">License name
          <input value={licenseName} placeholder="Apache-2.0" onChange={(e) => setLicenseName(e.target.value)} />
        </label>
        <label className="form-row">License URL
          <input value={licenseUrl} placeholder="https://…" onChange={(e) => setLicenseUrl(e.target.value)} />
        </label>
      </div>

      <div className="eyebrow">Icon</div>
      <IconPicker
        value={existing.logo}
        valueDark={existing.logo_dark}
        inherited={familyInheritedIcon(familyId, families, genealogies)}
        onSelect={onSelectIcon}
        onClear={onClearIcon}
        onUpload={onUploadIcon}
      />

      <label className="form-row">Why this change? (optional)
        <input value={reason} placeholder="e.g. corrected creator after Sprint A logo audit" onChange={(e) => setReason(e.target.value)} />
      </label>

      <div className="form-actions" style={{ marginTop: 20 }}>
        <button type="button" className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton className="go" pending={pending} isError={!!error} disabled={pending || !name} onClick={submit} />
      </div>
    </div>
  );
}
