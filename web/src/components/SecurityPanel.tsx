import { useEffect, useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api";
import { appLocale } from "../lib/format";
import { SaveButton } from "./SaveButton";
import {
  useAuthConfig,
  useAuthPolicy,
  useHFToken,
  useIdentityLinkCreate,
  useIdentityLinkDelete,
  useIdentityLinks,
  useKeyCreate,
  useKeyRevoke,
  useKeys,
  useRecoveryCodesGenerate,
  useRecoveryCodesStatus,
  useSetHFToken,
  useTOTPConfirm,
  useTOTPDelete,
  useTOTPEnroll,
  useUpdateAuthConfig,
  useUpdateAuthPolicy,
  useWebAuthnAssert,
  useWebAuthnCredentialDelete,
  useWebAuthnCredentials,
  useWebAuthnRegister,
} from "../lib/queries";
import { useSession } from "../lib/session";
import { useStepUpGate } from "../lib/useStepUpGate";
import type { APIKeyKind, AuthConfigPutRequest, AuthFactor, AuthNetworkProvider, Role } from "../lib/types";
import { DangerZone } from "../settings/DangerZone";
import { Field } from "../settings/Field";
import { SECURITY_DANGER_FIELDS } from "../settings/fields";
import { ConfirmButton } from "./ConfirmButton";
import { CopyButton } from "./CopyButton";
import { StepUpModal } from "./StepUpModal";

const SECURITY_DANGER_FIELDS_BY_ID = Object.fromEntries(SECURITY_DANGER_FIELDS.map((f) => [f.id, f]));

// SecurityPanel — the FE-6 Security tab (docs/v5-sprint0-auth-design.md §6).
// Config UI for the Access Policy & Auth v2 subsystem: policy matrix editor,
// auth config, API-key management, identity linking, TOTP enrollment, WebAuthn
// credentials, and recovery codes. Replaces the former SecurityPlaceholder.
//
// Mutations on this tab are gated by `requireAssurance(area.settings.security)`,
// which requires password-level assurance. When a mutation returns 403
// `step_up_required`, a StepUpModal is shown; after successful step-up the
// mutation is retried.

const POLICY_RESOURCES: { key: string; labelKey: string }[] = [
  { key: "page.dashboard", labelKey: "policy.res_page_dashboard" },
  { key: "page.console", labelKey: "policy.res_page_console" },
  { key: "page.scheduling", labelKey: "policy.res_page_scheduling" },
  { key: "page.compression", labelKey: "policy.res_page_compression" },
  { key: "page.settings", labelKey: "policy.res_page_settings" },
  // P3 — the Help page's "Ask the smith" chat + Diagnostics sub-tabs. Added
  // proactively alongside the reasoning-tier UI, same reasoning as
  // action.model.profile below: don't ship a new backend-gated resource
  // with no matrix row for it.
  { key: "page.smith", labelKey: "policy.res_page_smith" },
  { key: "area.settings.security", labelKey: "policy.res_area_security" },
  { key: "area.settings.provider_keys", labelKey: "policy.res_area_provider_keys" },
  // Sprint 12 (was H): the Danger Zone's own resource — boot-critical infra
  // (listen addresses, paths, ports, tailscale) + the daemon restart action.
  { key: "area.settings.system", labelKey: "policy.res_area_system" },
  { key: "action.system.restart", labelKey: "policy.res_action_restart" },
  { key: "action.model.load_unload", labelKey: "policy.res_action_load_unload" },
  { key: "action.compressor.teardown", labelKey: "policy.res_action_teardown" },
  { key: "action.reservation.write", labelKey: "policy.res_action_reservations" },
  // Real pre-existing gap found this sprint: this resource has existed in
  // Go since the PROFILE track (authz/policy.go) but was never added here,
  // so an admin could never actually configure its assurance level from
  // the UI despite the backend already gating on it.
  { key: "action.model.profile", labelKey: "policy.res_action_profile" },
  // Wave 3 / P2 (track W3-C) — gates POST /smith/actions (create) and
  // POST /smith/actions/{id}/approve. Added proactively alongside the
  // action-model UI so this resource doesn't repeat the action.model.profile
  // gap noted above (backend-gated for months with no matrix row).
  { key: "action.smith.execute", labelKey: "policy.res_action_smith_execute" },
  // Autonomous-remediation Sprint 5 (docs/v5-smith.md §13.3): gates PUT
  // /smith/autonomy's ESCALATING direction only (turning the global
  // autonomy kill switch on, or opting a procedure in) — added proactively
  // alongside the backend so this resource doesn't repeat the
  // action.model.profile gap noted above.
  { key: "action.smith.autonomy", labelKey: "policy.res_action_autonomy" },
];

const FACTORS: AuthFactor[] = ["network", "password", "totp", "passkey"];

const KEY_KINDS: APIKeyKind[] = ["forge", "router", "mcp"];
const ROLES: Role[] = ["viewer", "operator", "admin"];
const NETWORK_PROVIDERS: AuthNetworkProvider[] = ["tailscale", "forward_auth_header", "none"];

// useStepUpGate moved to lib/useStepUpGate.ts (Sprint 12 Phase 0) so other
// settings sections (SchedulerTunables, the Danger Zone) can share it.

// ── Policy matrix ─────────────────────────────────────────────────────────────

function PolicyMatrix({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("security");
  const policy = useAuthPolicy();
  const update = useUpdateAuthPolicy();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Record<string, AuthFactor> | null>(null);

  const current = draft ?? policy.data?.policy ?? {};
  const dirty = draft != null;

  function save() {
    if (!draft) return;
    setError(null);
    const doSave = () =>
      update.mutate({ policy: draft }, {
        onSuccess: () => setDraft(null),
        onError: (e) => { if (!gate.handle(e, save)) setError(apiErrorMessage(e)); },
      });
    doSave();
  }

  if (policy.isError) {
    return (
      <>
        <div className="eyebrow" id="security-policy">{t("policy.title")}</div>
        <div className="card"><div className="empty-note">{t("policy.unavailable")}</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow" id="security-policy">{t("policy.title_full")}</div>
      <div className="card">
        <div className="sec-hint">
          <Trans i18nKey="policy.hint" ns="security" components={{ b: <b /> }} />
        </div>
        {error && <div className="error-note sec-err">{error}</div>}
        <div className="qrow sec-qhead">
          <span className="sec-w240">{t("policy.col_resource")}</span>
          <span className="sec-w150">{t("policy.col_min_factor")}</span>
        </div>
        {POLICY_RESOURCES.map((r) => (
          <div className="qrow" key={r.key}>
            <span className="sec-w240 sec-cell">{t(r.labelKey)}</span>
            <select
              className="sec-w150"
              value={current[r.key] ?? "network"}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...current, [r.key]: e.target.value as AuthFactor })}
            >
              {FACTORS.map((f) => <option key={f} value={f}>{f}</option>)}
            </select>
          </div>
        ))}
        {canAdmin && dirty && (
          <div className="form-actions sec-top">
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>{t("shared.reset")}</button>
            <button className="btn primary" disabled={update.isPending} onClick={save}>{t("policy.save")}</button>
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Auth config ──────────────────────────────────────────────────────────────

function AuthConfigSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("security");
  const cfg = useAuthConfig();
  const update = useUpdateAuthConfig();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Partial<AuthConfigPutRequest> | null>(null);

  function save() {
    if (!draft) return;
    setError(null);
    const doSave = () =>
      update.mutate(draft, {
        onSuccess: () => setDraft(null),
        onError: (e) => { if (!gate.handle(e, save)) setError(apiErrorMessage(e)); },
      });
    doSave();
  }

  if (cfg.isError) return (
    <>
      <div className="eyebrow" id="security-auth-config">{t("auth.title")}</div>
      <div className="card"><div className="empty-note">{t("auth.unavailable")}</div></div>
    </>
  );
  if (!cfg.data) return (
    <>
      <div className="eyebrow" id="security-auth-config">{t("auth.title")}</div>
      <div className="card"><div className="empty-note">{t("auth.loading")}</div></div>
    </>
  );

  const active = { ...cfg.data, ...draft };
  const dirty = draft != null;

  return (
    <>
      <div className="eyebrow" id="security-auth-config">{t("auth.title")}</div>
      <div className="card">
        {error && <div className="error-note sec-err">{error}</div>}
        <div className="form-grid">
          <label className="form-row">{t("auth.network_provider")}
            <select
              value={String(active.network_provider ?? "none")}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, network_provider: e.target.value as AuthNetworkProvider })}
            >
              {NETWORK_PROVIDERS.map((p) => <option key={p} value={p}>{p}</option>)}
            </select>
          </label>
          <label className="form-row">{t("auth.network_role")}
            <select
              value={String(active.network_default_role ?? "viewer")}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, network_default_role: e.target.value as Role })}
            >
              {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </label>
          <label className="form-row">{t("auth.step_up_ttl")}
            <input
              type="number"
              min={1}
              value={Number(active.step_up_ttl_min ?? 15)}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, step_up_ttl_min: parseInt(e.target.value, 10) })}
            />
          </label>
          <label className="form-row">{t("auth.webauthn_rp_id")}
            <input
              value={String(active.webauthn_rp_id ?? "")}
              placeholder="example-tailnet.ts.net"
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, webauthn_rp_id: e.target.value })}
            />
          </label>
          <label className="form-row">{t("auth.webauthn_rp_name")}
            <input
              value={String(active.webauthn_rp_name ?? "")}
              placeholder="Forge"
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, webauthn_rp_name: e.target.value })}
            />
          </label>
          <label className="form-row sec-check-row">
            <input
              type="checkbox"
              checked={Boolean(active.a0_tailnet_bypass ?? true)}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, a0_tailnet_bypass: e.target.checked })}
            />
            {t("auth.a0_bypass")}
          </label>
        </div>
        {canAdmin && dirty && (
          <div className="form-actions sec-top">
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>{t("shared.reset")}</button>
            <button className="btn primary" disabled={update.isPending} onClick={save}>{t("auth.save")}</button>
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Forward-auth keys (embedded Danger Zone, Sprint 12 Phase 7) ────────────
//
// The two auth.provider.forward_auth_header.* keys are meaningless outside
// the network_provider selector above — they only apply when
// network_provider is (or is becoming) "forward_auth_header" — so they get
// their own small Danger Zone instance here rather than living in the
// standalone "danger" section (settings/panels/Danger.tsx). No onCheck
// prop: unlike the system-settings group, there's no separate dry-run
// preflight endpoint for these — the highest-value check (would this save
// lock the SAVING operator out) runs inline inside PUT /auth/config itself
// (auth_handlers.go's checkTrustedCIDRLockout) and 422s with the same
// {error:"preflight_failed", checks} shape DangerZone already knows how to
// render from a failed Save.
interface ForwardAuthDraft {
  header_name: string;
  trusted_cidrs: string;
}

function ForwardAuthDangerZone({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("security");
  const cfg = useAuthConfig();
  const update = useUpdateAuthConfig();

  const pc = (cfg.data?.provider_config ?? {}) as { header_name?: string; trusted_cidrs?: string };
  const data: ForwardAuthDraft | undefined = cfg.data
    ? { header_name: pc.header_name ?? "X-Auth-Request-User", trusted_cidrs: pc.trusted_cidrs ?? "127.0.0.0/8" }
    : undefined;

  return (
    <DangerZone<ForwardAuthDraft>
      title={t("forward_auth.title")}
      canAdmin={canAdmin}
      isError={cfg.isError}
      data={data}
      blurb={
        <Trans i18nKey="forward_auth.blurb" ns="security" components={{ b: <b />, code: <code /> }} />
      }
      onSave={(draft) =>
        update.mutateAsync({ provider_config: { header_name: draft.header_name, trusted_cidrs: draft.trusted_cidrs } })
      }
      renderFields={(active, setField, disabled) => (
        <div className="form-grid wide">
          <Field
            rec={SECURITY_DANGER_FIELDS_BY_ID["security.forward_auth.header_name"]}
            value={active.header_name}
            disabled={disabled}
            onChange={(v) => setField("header_name", String(v))}
          />
          <Field
            rec={SECURITY_DANGER_FIELDS_BY_ID["security.forward_auth.trusted_cidrs"]}
            value={active.trusted_cidrs}
            disabled={disabled}
            onChange={(v) => setField("trusted_cidrs", String(v))}
          />
        </div>
      )}
    />
  );
}

// ── HF token (model acquisition) ────────────────────────────────────────────
// Follows the same masked-value-means-unchanged contract as smith's web-
// provider keys (resolveAPIKeyWrite, go/internal/httpapi/hf_handlers.go):
// re-submitting the masked value read from GET is a no-op, never a clobber.

function HFTokenSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("security");
  const token = useHFToken();
  const setToken = useSetHFToken();
  const [value, setValue] = useState("");

  useEffect(() => {
    if (token.data) setValue(token.data.token);
  }, [token.data]);

  return (
    <>
      <div className="eyebrow" id="security-hf-token">{t("hf_token.title")}</div>
      <div className="card">
        <div className="sec-hint">{t("hf_token.hint")}</div>
        {setToken.isError && <div className="error-note sec-err">{apiErrorMessage(setToken.error)}</div>}
        <div className="sec-row">
          <label className="form-row sec-grow-260">
            <input
              type="password"
              value={value}
              disabled={!canAdmin}
              onChange={(e) => setValue(e.target.value)}
              placeholder={token.data?.configured ? t("hf_token.configured") : t("hf_token.not_configured")}
            />
          </label>
          {canAdmin && (
            <SaveButton pending={setToken.isPending} isError={setToken.isError} onClick={() => setToken.mutate(value)} />
          )}
        </div>
      </div>
    </>
  );
}

// ── API keys ────────────────────────────────────────────────────────────────

function APIKeys({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("security");
  const keys = useKeys();
  const create = useKeyCreate();
  const revoke = useKeyRevoke();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [draftKind, setDraftKind] = useState<APIKeyKind>("forge");
  const [draftName, setDraftName] = useState("");
  const [draftDisplayName, setDraftDisplayName] = useState("");
  const [draftRole, setDraftRole] = useState<Role>("viewer");
  const [showRevoked, setShowRevoked] = useState(false);

  function submitCreate() {
    setError(null);
    const doCreate = () =>
      create.mutate(
        { kind: draftKind, name: draftName, display_name: draftDisplayName || undefined, role: draftKind === "forge" ? draftRole : undefined },
        {
          onSuccess: (data) => { setNewToken(data.token); setCreating(false); setDraftName(""); setDraftDisplayName(""); },
          onError: (e) => { if (!gate.handle(e, doCreate)) setError(apiErrorMessage(e)); },
        },
      );
    doCreate();
  }

  // Sprint K: confirm() gate moved into ConfirmButton at the call site —
  // the gate.handle step-up retry below is unaffected, it only wraps
  // doRevoke's own error path.
  function submitRevoke(keyid: string) {
    setError(null);
    const doRevoke = () =>
      revoke.mutate(keyid, {
        onError: (e) => { if (!gate.handle(e, doRevoke)) setError(apiErrorMessage(e)); },
      });
    doRevoke();
  }

  const allKeys = keys.data?.keys ?? [];
  // revoked_at is omitempty on a *float64 server-side — an active key OMITS
  // the field entirely (undefined), it isn't sent as null. `!= null` catches
  // both undefined and null, so it's correct regardless of which shows up.
  const revokedCount = allKeys.filter((k) => k.revoked_at != null).length;
  const list = showRevoked ? allKeys : allKeys.filter((k) => k.revoked_at == null);

  return (
    <>
      <div className="eyebrow" id="security-api-keys">{t("api_keys.title")}</div>
      <div className="card">
        <div className="sec-hint">
          <Trans i18nKey="api_keys.hint" ns="security" components={{ b: <b /> }} />
        </div>
        {error && <div className="error-note sec-err">{error}</div>}
        {newToken && (
          <div className="card sec-reveal-card">
            <div className="sec-mini">{t("api_keys.new_key_label")}</div>
            <code className="sec-token">{newToken}</code>
            <div className="sec-copy-row">
              <CopyButton text={newToken} label={t("shared.copy")} />
              <button className="btn sec-inline-gap" onClick={() => setNewToken(null)}>{t("shared.dismiss")}</button>
            </div>
          </div>
        )}
        {keys.isLoading && <div className="empty-note">{t("api_keys.loading")}</div>}
        {list.length === 0 && !keys.isLoading && (
          <div className="empty-note">
            {allKeys.length === 0 ? t("api_keys.none") : t("api_keys.none_active")}
          </div>
        )}
        {revokedCount > 0 && (
          <div className="sec-revoked-toggle">
            <button className="btn sec-btn-xs" onClick={() => setShowRevoked((v) => !v)}>
              {showRevoked ? t("api_keys.hide_revoked") : t("api_keys.show_revoked", { count: revokedCount })}
            </button>
          </div>
        )}
        {list.length > 0 && (
          <>
            <div className="qrow sec-qhead">
              <span className="sec-w120">{t("api_keys.col_id")}</span>
              <span className="sec-w70">{t("api_keys.col_kind")}</span>
              <span className="sec-w140">{t("api_keys.col_name")}</span>
              <span className="sec-w70">{t("api_keys.col_role")}</span>
              <span className="sec-w90">{t("api_keys.col_last_used")}</span>
              <span className="pos">{t("shared.actions")}</span>
            </div>
            {list.map((k) => (
              <div className="qrow" key={k.keyid}>
                <span className="sec-w120 sec-mono-xs">{k.keyid}</span>
                <span className="sec-w70 sec-cell">{k.kind}</span>
                <span className="sec-w140 sec-cell">{k.name}</span>
                <span className="sec-w70 sec-cell">{k.role ?? "—"}</span>
                <span className="sec-w90 sec-cell sec-dim">
                  {k.last_used_at ? new Date(k.last_used_at * 1000).toLocaleDateString(appLocale()) : t("shared.never")}
                </span>
                <span className="pos">
                  {k.revoked_at ? (
                    <span className="sec-revoked">{t("shared.revoked")}</span>
                  ) : canAdmin ? (
                    <ConfirmButton
                      pending={revoke.isPending}
                      onConfirm={() => submitRevoke(k.keyid)}
                      label={t("api_keys.revoke")}
                      warning={t("api_keys.revoke_warning", { keyid: k.keyid })}
                    />
                  ) : null}
                </span>
              </div>
            ))}
          </>
        )}
        {canAdmin && (
          creating ? (
            <div className="form-grid sec-top">
              <label className="form-row">{t("api_keys.field_kind")}
                <select value={draftKind} onChange={(e) => setDraftKind(e.target.value as APIKeyKind)}>
                  {KEY_KINDS.map((k) => <option key={k} value={k}>{k}</option>)}
                </select>
              </label>
              <label className="form-row">{t("api_keys.field_name")}
                <input value={draftName} placeholder="opencode" onChange={(e) => setDraftName(e.target.value)} />
              </label>
              <label className="form-row">{t("api_keys.field_display_name")}
                <input value={draftDisplayName} placeholder={t("api_keys.field_display_name_ph")} onChange={(e) => setDraftDisplayName(e.target.value)} />
              </label>
              {draftKind === "forge" && (
                <label className="form-row">{t("api_keys.field_role")}
                  <select value={draftRole} onChange={(e) => setDraftRole(e.target.value as Role)}>
                    {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
                  </select>
                </label>
              )}
              <div className="form-actions sec-grid-actions">
                <button className="btn" onClick={() => { setCreating(false); setDraftName(""); }}>{t("shared.cancel")}</button>
                <button className="btn primary" disabled={create.isPending || !draftName} onClick={submitCreate}>{t("api_keys.mint")}</button>
              </div>
            </div>
          ) : (
            <button className="btn sec-top" onClick={() => { setCreating(true); setError(null); }}>
              {t("api_keys.mint_add")}
            </button>
          )
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Identity links ───────────────────────────────────────────────────────────

function IdentityLinks({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("security");
  const links = useIdentityLinks();
  const create = useIdentityLinkCreate();
  const remove = useIdentityLinkDelete();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [draftProvider, setDraftProvider] = useState<AuthNetworkProvider>("tailscale");
  const [draftPrincipal, setDraftPrincipal] = useState("");
  const [draftUserID, setDraftUserID] = useState(1);

  function submitCreate() {
    setError(null);
    const doCreate = () =>
      create.mutate(
        { provider: draftProvider, principal: draftPrincipal, user_id: draftUserID },
        {
          onSuccess: () => { setCreating(false); setDraftPrincipal(""); },
          onError: (e) => { if (!gate.handle(e, doCreate)) setError(apiErrorMessage(e)); },
        },
      );
    doCreate();
  }

  // Sprint K: this delete had NO confirmation at all before — arming it via
  // ConfirmButton at the call site is a small real improvement, not just a
  // style migration like the other three SecurityPanel deletes.
  function submitDelete(provider: string, principal: string) {
    setError(null);
    const doDelete = () =>
      remove.mutate(
        { provider, principal },
        { onError: (e) => { if (!gate.handle(e, doDelete)) setError(apiErrorMessage(e)); } },
      );
    doDelete();
  }

  const list = links.data?.links ?? [];

  return (
    <>
      <div className="eyebrow" id="security-identity-links">{t("identity.title")}</div>
      <div className="card">
        <div className="sec-hint">{t("identity.hint")}</div>
        {error && <div className="error-note sec-err">{error}</div>}
        {list.length === 0 && !links.isLoading && <div className="empty-note">{t("identity.none")}</div>}
        {list.length > 0 && (
          <>
            <div className="qrow sec-qhead">
              <span className="sec-w150">{t("identity.col_provider")}</span>
              <span className="sec-w200">{t("identity.col_principal")}</span>
              <span className="sec-w60">{t("identity.col_user_id")}</span>
              <span className="pos">{t("shared.actions")}</span>
            </div>
            {list.map((l) => (
              <div className="qrow" key={`${l.provider}:${l.principal}`}>
                <span className="sec-w150 sec-cell">{l.provider}</span>
                <span className="sec-w200 sec-mono-xs">{l.principal}</span>
                <span className="sec-w60 sec-mono-sm">{l.user_id}</span>
                <span className="pos">
                  {canAdmin && (
                    <ConfirmButton
                      pending={remove.isPending}
                      onConfirm={() => submitDelete(l.provider, l.principal)}
                      label={t("identity.unlink")}
                      warning={t("identity.unlink_warning", { provider: l.provider, principal: l.principal })}
                    />
                  )}
                </span>
              </div>
            ))}
          </>
        )}
        {canAdmin && (
          creating ? (
            <div className="form-grid sec-top">
              <label className="form-row">{t("identity.field_provider")}
                <select value={draftProvider} onChange={(e) => setDraftProvider(e.target.value as AuthNetworkProvider)}>
                  {NETWORK_PROVIDERS.filter((p) => p !== "none").map((p) => <option key={p} value={p}>{p}</option>)}
                </select>
              </label>
              <label className="form-row">{t("identity.field_principal")}
                <input value={draftPrincipal} placeholder="user@github" onChange={(e) => setDraftPrincipal(e.target.value)} />
              </label>
              <label className="form-row">{t("identity.field_user_id")}
                <input
                  type="number"
                  min={1}
                  value={draftUserID}
                  onChange={(e) => setDraftUserID(Number(e.target.value))}
                />
              </label>
              <div className="form-actions sec-grid-actions">
                <button className="btn" onClick={() => { setCreating(false); setDraftPrincipal(""); }}>{t("shared.cancel")}</button>
                <button className="btn primary" disabled={create.isPending || !draftPrincipal} onClick={submitCreate}>{t("identity.link")}</button>
              </div>
            </div>
          ) : (
            <button className="btn sec-top" onClick={() => { setCreating(true); setError(null); }}>
              {t("identity.add")}
            </button>
          )
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── TOTP ─────────────────────────────────────────────────────────────────────

function TOTPSection() {
  const { t } = useTranslation("security");
  const enroll = useTOTPEnroll();
  const confirm = useTOTPConfirm();
  const remove = useTOTPDelete();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [enrollData, setEnrollData] = useState<{ secret: string; otpauth_uri: string } | null>(null);
  const [confirmCode, setConfirmCode] = useState("");

  function startEnroll() {
    setError(null);
    const doEnroll = () =>
      enroll.mutate(undefined, {
        onSuccess: (data) => setEnrollData(data),
        onError: (e) => { if (!gate.handle(e, doEnroll)) setError(apiErrorMessage(e)); },
      });
    doEnroll();
  }

  function submitConfirm() {
    setError(null);
    const doConfirm = () =>
      confirm.mutate(
        { code: confirmCode },
        {
          onSuccess: () => { setEnrollData(null); setConfirmCode(""); },
          onError: (e) => { if (!gate.handle(e, doConfirm)) setError(apiErrorMessage(e)); },
        },
      );
    doConfirm();
  }

  function submitDelete() {
    setError(null);
    const doDelete = () =>
      remove.mutate(undefined, {
        onError: (e) => { if (!gate.handle(e, doDelete)) setError(apiErrorMessage(e)); },
      });
    doDelete();
  }

  return (
    <>
      <div className="eyebrow" id="security-totp">{t("totp.title")}</div>
      <div className="card">
        {error && <div className="error-note sec-err">{error}</div>}
        {enrollData ? (
          <div>
            <div className="sec-note">{t("totp.scan_hint")}</div>
            <div className="sec-secret">{enrollData.secret}</div>
            <div className="sec-otp-label">
              otpauth: <span className="sec-mono">{enrollData.otpauth_uri}</span>
            </div>
            <div className="sec-row end">
              <label className="form-row sec-grow-200">{t("totp.confirm_code")}
                <input
                  value={confirmCode}
                  onChange={(e) => setConfirmCode(e.target.value)}
                  placeholder="123456"
                  maxLength={8}
                  autoComplete="one-time-code"
                  autoFocus
                />
              </label>
              <div className="form-actions">
                <button className="btn" onClick={() => { setEnrollData(null); setConfirmCode(""); }}>{t("shared.cancel")}</button>
                <button className="btn primary" disabled={confirm.isPending || !confirmCode} onClick={submitConfirm}>{t("totp.confirm")}</button>
              </div>
            </div>
          </div>
        ) : (
          <div className="sec-btnrow">
            <button className="btn" disabled={enroll.isPending} onClick={startEnroll}>
              {enroll.isPending ? t("totp.enrolling") : t("totp.enroll")}
            </button>
            <ConfirmButton
              pending={remove.isPending}
              onConfirm={submitDelete}
              label={t("totp.remove")}
              warning={t("totp.remove_warning")}
            />
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── WebAuthn credentials ────────────────────────────────────────────────────

function WebAuthnSection() {
  const { t } = useTranslation("security");
  const creds = useWebAuthnCredentials();
  const register = useWebAuthnRegister();
  const assert = useWebAuthnAssert();
  const remove = useWebAuthnCredentialDelete();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);

  function startRegister() {
    setError(null);
    const doRegister = () =>
      register.mutate("passkey", {
        onError: (e) => { if (!gate.handle(e, doRegister)) setError(apiErrorMessage(e)); },
      });
    doRegister();
  }

  function startAssert() {
    setError(null);
    assert.mutate(undefined, {
      onError: (e) => { if (!gate.handle(e, startAssert)) setError(apiErrorMessage(e)); },
    });
  }

  function submitDelete(id: string) {
    setError(null);
    const doDelete = () =>
      remove.mutate(id, {
        onError: (e) => { if (!gate.handle(e, doDelete)) setError(apiErrorMessage(e)); },
      });
    doDelete();
  }

  const list = creds.data?.credentials ?? [];

  return (
    <>
      <div className="eyebrow" id="security-webauthn">{t("webauthn.title")}</div>
      <div className="card">
        <div className="sec-hint">{t("webauthn.hint")}</div>
        {error && <div className="error-note sec-err">{error}</div>}
        {list.length === 0 && !creds.isLoading && <div className="empty-note">{t("webauthn.none")}</div>}
        {list.length > 0 && (
          <>
            <div className="qrow sec-qhead">
              <span className="sec-w200">{t("webauthn.col_credential")}</span>
              <span className="sec-w100">{t("webauthn.col_label")}</span>
              <span className="sec-w100">{t("webauthn.col_last_used")}</span>
              <span className="pos">{t("shared.actions")}</span>
            </div>
            {list.map((c) => (
              <div className="qrow" key={c.id}>
                <span className="sec-w200 sec-mono-2xs">{c.id.slice(0, 24)}…</span>
                <span className="sec-w100 sec-cell">{c.label || "—"}</span>
                <span className="sec-w100 sec-cell sec-dim">
                  {c.last_used_at ? new Date(c.last_used_at * 1000).toLocaleDateString(appLocale()) : t("shared.never")}
                </span>
                <span className="pos">
                  <ConfirmButton pending={remove.isPending} onConfirm={() => submitDelete(c.id)} label={t("webauthn.remove")} warning={t("webauthn.remove_warning")} />
                </span>
              </div>
            ))}
          </>
        )}
        <div className="sec-btnrow top">
          <button className="btn" disabled={register.isPending} onClick={startRegister}>
            {register.isPending ? t("webauthn.registering") : t("webauthn.register")}
          </button>
          <button className="btn" disabled={assert.isPending} onClick={startAssert}>
            {assert.isPending ? t("webauthn.verifying") : t("webauthn.step_up")}
          </button>
        </div>
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Recovery codes ──────────────────────────────────────────────────────────

function RecoveryCodes() {
  const { t } = useTranslation("security");
  const status = useRecoveryCodesStatus();
  const generate = useRecoveryCodesGenerate();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);

  function submitGenerate() {
    setError(null);
    const doGen = () =>
      generate.mutate(undefined, {
        onSuccess: (data) => setCodes(data.codes),
        onError: (e) => { if (!gate.handle(e, doGen)) setError(apiErrorMessage(e)); },
      });
    doGen();
  }

  return (
    <>
      <div className="eyebrow" id="security-recovery-codes">{t("recovery.title")}</div>
      <div className="card">
        <div className="sec-hint">{t("recovery.hint")}</div>
        {error && <div className="error-note sec-err">{error}</div>}
        {status.data && (
          <div className="sec-note">
            {status.data.has_codes
              ? t("recovery.codes_unused", { unused: status.data.unused, total: status.data.total })
              : t("recovery.none_yet")}
          </div>
        )}
        {codes && (
          <div className="card sec-reveal-card">
            <div className="sec-mini">{t("recovery.save_now")}</div>
            <div className="sec-codes">
              {codes.map((c) => <div key={c}>{c}</div>)}
            </div>
            <div className="sec-copy-row">
              <CopyButton text={codes.join("\n")} label={t("recovery.copy_all")} />
              <button className="btn sec-inline-gap" onClick={() => setCodes(null)}>{t("shared.dismiss")}</button>
            </div>
          </div>
        )}
        <button className="btn" disabled={generate.isPending} onClick={submitGenerate}>
          {generate.isPending ? t("recovery.generating") : codes ? t("recovery.regenerate") : t("recovery.generate")}
        </button>
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Panel ────────────────────────────────────────────────────────────────────

export function SecurityPanel({ canAdmin }: { canAdmin: boolean }) {
  const { canOperate } = useSession();
  void canOperate;
  return (
    <>
      <PolicyMatrix canAdmin={canAdmin} />
      <AuthConfigSection canAdmin={canAdmin} />
      <ForwardAuthDangerZone canAdmin={canAdmin} />
      <HFTokenSection canAdmin={canAdmin} />
      <APIKeys canAdmin={canAdmin} />
      <IdentityLinks canAdmin={canAdmin} />
      <TOTPSection />
      <WebAuthnSection />
      <RecoveryCodes />
    </>
  );
}
