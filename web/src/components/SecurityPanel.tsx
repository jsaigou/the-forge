import { useEffect, useState } from "react";
import { apiErrorMessage } from "../lib/api";
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

const POLICY_RESOURCES: { key: string; label: string }[] = [
  { key: "page.dashboard", label: "Dashboard" },
  { key: "page.console", label: "Console" },
  { key: "page.scheduling", label: "Scheduling" },
  { key: "page.compression", label: "Compressor" },
  { key: "page.settings", label: "Settings" },
  // P3 — the Help page's "Ask the smith" chat + Diagnostics sub-tabs. Added
  // proactively alongside the reasoning-tier UI, same reasoning as
  // action.model.profile below: don't ship a new backend-gated resource
  // with no matrix row for it.
  { key: "page.smith", label: "Help → Ask the smith" },
  { key: "area.settings.security", label: "Settings → Security" },
  { key: "area.settings.provider_keys", label: "Settings → Providers" },
  // Sprint 12 (was H): the Danger Zone's own resource — boot-critical infra
  // (listen addresses, paths, ports, tailscale) + the daemon restart action.
  { key: "area.settings.system", label: "Settings → Danger Zone" },
  { key: "action.system.restart", label: "Restart daemon" },
  { key: "action.model.load_unload", label: "Load / unload models" },
  { key: "action.compressor.teardown", label: "Compressor teardown" },
  { key: "action.reservation.write", label: "Reservations" },
  // Real pre-existing gap found this sprint: this resource has existed in
  // Go since the PROFILE track (authz/policy.go) but was never added here,
  // so an admin could never actually configure its assurance level from
  // the UI despite the backend already gating on it.
  { key: "action.model.profile", label: "Run model profiling" },
  // Wave 3 / P2 (track W3-C) — gates POST /smith/actions (create) and
  // POST /smith/actions/{id}/approve. Added proactively alongside the
  // action-model UI so this resource doesn't repeat the action.model.profile
  // gap noted above (backend-gated for months with no matrix row).
  { key: "action.smith.execute", label: "Approve smith actions" },
  // Autonomous-remediation Sprint 5 (docs/v5-smith.md §13.3): gates PUT
  // /smith/autonomy's ESCALATING direction only (turning the global
  // autonomy kill switch on, or opting a procedure in) — added proactively
  // alongside the backend so this resource doesn't repeat the
  // action.model.profile gap noted above.
  { key: "action.smith.autonomy", label: "Standing autonomy policy" },
];

const FACTORS: AuthFactor[] = ["network", "password", "totp", "passkey"];

const KEY_KINDS: APIKeyKind[] = ["forge", "router", "mcp"];
const ROLES: Role[] = ["viewer", "operator", "admin"];
const NETWORK_PROVIDERS: AuthNetworkProvider[] = ["tailscale", "forward_auth_header", "none"];

// useStepUpGate moved to lib/useStepUpGate.ts (Sprint 12 Phase 0) so other
// settings sections (SchedulerTunables, the Danger Zone) can share it.

// ── Policy matrix ─────────────────────────────────────────────────────────────

function PolicyMatrix({ canAdmin }: { canAdmin: boolean }) {
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
        <div className="eyebrow" id="security-policy">Access policy</div>
        <div className="card"><div className="empty-note">Policy store unavailable.</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow" id="security-policy">Access policy · resource → minimum factor</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Each resource requires at least the selected factor. <b>network</b> = tailnet membership suffices;
          <b> password</b>/<b>totp</b> = step-up needed; <b>passkey</b> = hardware/biometric (L2).
          Orthogonal to RBAC role (both must pass).
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
          <span style={{ width: 240 }}>Resource</span>
          <span style={{ width: 150 }}>Minimum factor</span>
        </div>
        {POLICY_RESOURCES.map((r) => (
          <div className="qrow" key={r.key}>
            <span style={{ width: 240, fontSize: 12 }}>{r.label}</span>
            <select
              style={{ width: 150 }}
              value={current[r.key] ?? "network"}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...current, [r.key]: e.target.value as AuthFactor })}
            >
              {FACTORS.map((f) => <option key={f} value={f}>{f}</option>)}
            </select>
          </div>
        ))}
        {canAdmin && dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>Reset</button>
            <button className="btn primary" disabled={update.isPending} onClick={save}>Save policy</button>
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Auth config ──────────────────────────────────────────────────────────────

function AuthConfigSection({ canAdmin }: { canAdmin: boolean }) {
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
      <div className="eyebrow" id="security-auth-config">Auth configuration</div>
      <div className="card"><div className="empty-note">Auth config unavailable.</div></div>
    </>
  );
  if (!cfg.data) return (
    <>
      <div className="eyebrow" id="security-auth-config">Auth configuration</div>
      <div className="card"><div className="empty-note">Loading auth config…</div></div>
    </>
  );

  const active = { ...cfg.data, ...draft };
  const dirty = draft != null;

  return (
    <>
      <div className="eyebrow" id="security-auth-config">Auth configuration</div>
      <div className="card">
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-grid">
          <label className="form-row">Network identity provider
            <select
              value={String(active.network_provider ?? "none")}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, network_provider: e.target.value as AuthNetworkProvider })}
            >
              {NETWORK_PROVIDERS.map((p) => <option key={p} value={p}>{p}</option>)}
            </select>
          </label>
          <label className="form-row">Network default role
            <select
              value={String(active.network_default_role ?? "viewer")}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, network_default_role: e.target.value as Role })}
            >
              {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </label>
          <label className="form-row">Step-up TTL (minutes)
            <input
              type="number"
              min={1}
              value={Number(active.step_up_ttl_min ?? 15)}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, step_up_ttl_min: parseInt(e.target.value, 10) })}
            />
          </label>
          <label className="form-row">WebAuthn RP ID
            <input
              value={String(active.webauthn_rp_id ?? "")}
              placeholder="example-tailnet.ts.net"
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, webauthn_rp_id: e.target.value })}
            />
          </label>
          <label className="form-row">WebAuthn RP name
            <input
              value={String(active.webauthn_rp_name ?? "")}
              placeholder="Forge"
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, webauthn_rp_name: e.target.value })}
            />
          </label>
          <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
            <input
              type="checkbox"
              checked={Boolean(active.a0_tailnet_bypass ?? true)}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...draft, a0_tailnet_bypass: e.target.checked })}
            />
            a0 tailnet bypass
          </label>
        </div>
        {canAdmin && dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>Reset</button>
            <button className="btn primary" disabled={update.isPending} onClick={save}>Save config</button>
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
  const cfg = useAuthConfig();
  const update = useUpdateAuthConfig();

  const pc = (cfg.data?.provider_config ?? {}) as { header_name?: string; trusted_cidrs?: string };
  const data: ForwardAuthDraft | undefined = cfg.data
    ? { header_name: pc.header_name ?? "X-Auth-Request-User", trusted_cidrs: pc.trusted_cidrs ?? "127.0.0.0/8" }
    : undefined;

  return (
    <DangerZone<ForwardAuthDraft>
      title="Forward-auth keys"
      canAdmin={canAdmin}
      isError={cfg.isError}
      data={data}
      blurb={
        <>
          Only meaningful once <b>network provider</b> above is set to <code>forward_auth_header</code>. A save
          that would exclude your own address from <b>trusted CIDRs</b> is refused outright — total lockout from
          a forward-auth deployment has no recovery path short of direct database access.
        </>
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
  const token = useHFToken();
  const setToken = useSetHFToken();
  const [value, setValue] = useState("");

  useEffect(() => {
    if (token.data) setValue(token.data.token);
  }, [token.data]);

  return (
    <>
      <div className="eyebrow" id="security-hf-token">HuggingFace access token</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Needed to download gated models (license click-through repos, e.g. Gemma, Llama). Write-only —
          the real value is never shown once set.
        </div>
        {setToken.isError && <div className="error-note" style={{ marginBottom: 10 }}>{apiErrorMessage(setToken.error)}</div>}
        <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
          <label className="form-row" style={{ flex: "1 1 260px" }}>
            <input
              type="password"
              value={value}
              disabled={!canAdmin}
              onChange={(e) => setValue(e.target.value)}
              placeholder={token.data?.configured ? "configured" : "not configured"}
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
      <div className="eyebrow" id="security-api-keys">API keys</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Bearer keys for machine consumers (a0 router, MCP, dashboard API). The token is shown <b>once</b>
          on creation and never again — copy it immediately.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {newToken && (
          <div className="card" style={{ background: "var(--bg-2)", marginBottom: 12, padding: 12 }}>
            <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 4 }}>New key (shown once):</div>
            <code style={{ fontSize: 12, wordBreak: "break-all", fontFamily: "var(--mono)" }}>{newToken}</code>
            <div style={{ marginTop: 8 }}>
              <CopyButton text={newToken} label="Copy" />
              <button className="btn" style={{ marginLeft: 8 }} onClick={() => setNewToken(null)}>Dismiss</button>
            </div>
          </div>
        )}
        {keys.isLoading && <div className="empty-note">Loading keys…</div>}
        {list.length === 0 && !keys.isLoading && (
          <div className="empty-note">
            {allKeys.length === 0 ? "No keys configured." : "No active keys."}
          </div>
        )}
        {revokedCount > 0 && (
          <div style={{ marginBottom: 10 }}>
            <button className="btn" style={{ fontSize: 11 }} onClick={() => setShowRevoked((v) => !v)}>
              {showRevoked ? "Hide revoked" : `Show revoked (${revokedCount})`}
            </button>
          </div>
        )}
        {list.length > 0 && (
          <>
            <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
              <span style={{ width: 120 }}>Key ID</span>
              <span style={{ width: 70 }}>Kind</span>
              <span style={{ width: 140 }}>Name</span>
              <span style={{ width: 70 }}>Role</span>
              <span style={{ width: 90 }}>Last used</span>
              <span className="pos">Actions</span>
            </div>
            {list.map((k) => (
              <div className="qrow" key={k.keyid}>
                <span style={{ width: 120, fontFamily: "var(--mono)", fontSize: 11 }}>{k.keyid}</span>
                <span style={{ width: 70, fontSize: 12 }}>{k.kind}</span>
                <span style={{ width: 140, fontSize: 12 }}>{k.name}</span>
                <span style={{ width: 70, fontSize: 12 }}>{k.role ?? "—"}</span>
                <span style={{ width: 90, fontSize: 12, color: "var(--text-dim)" }}>
                  {k.last_used_at ? new Date(k.last_used_at * 1000).toLocaleDateString() : "never"}
                </span>
                <span className="pos">
                  {k.revoked_at ? (
                    <span style={{ color: "var(--crit)" }}>revoked</span>
                  ) : canAdmin ? (
                    <ConfirmButton
                      pending={revoke.isPending}
                      onConfirm={() => submitRevoke(k.keyid)}
                      label="Revoke"
                      warning={`Revoke key ${k.keyid}?`}
                    />
                  ) : null}
                </span>
              </div>
            ))}
          </>
        )}
        {canAdmin && (
          creating ? (
            <div className="form-grid" style={{ marginTop: 14 }}>
              <label className="form-row">Kind
                <select value={draftKind} onChange={(e) => setDraftKind(e.target.value as APIKeyKind)}>
                  {KEY_KINDS.map((k) => <option key={k} value={k}>{k}</option>)}
                </select>
              </label>
              <label className="form-row">Name
                <input value={draftName} placeholder="opencode" onChange={(e) => setDraftName(e.target.value)} />
              </label>
              <label className="form-row">Preferred label
                <input value={draftDisplayName} placeholder={'ExampleHost (OpenCode) — shown on slot attribution'} onChange={(e) => setDraftDisplayName(e.target.value)} />
              </label>
              {draftKind === "forge" && (
                <label className="form-row">Role
                  <select value={draftRole} onChange={(e) => setDraftRole(e.target.value as Role)}>
                    {ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
                  </select>
                </label>
              )}
              <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn" onClick={() => { setCreating(false); setDraftName(""); }}>Cancel</button>
                <button className="btn primary" disabled={create.isPending || !draftName} onClick={submitCreate}>Mint key</button>
              </div>
            </div>
          ) : (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setCreating(true); setError(null); }}>
              + Mint key
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
      <div className="eyebrow" id="security-identity-links">Identity links · network → account</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Links a trusted network principal (e.g. a Tailscale login) to a local account, so the user gets
          a named session with no password prompt. Admins see all links; others see only their own.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {list.length === 0 && !links.isLoading && <div className="empty-note">No identity links.</div>}
        {list.length > 0 && (
          <>
            <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
              <span style={{ width: 150 }}>Provider</span>
              <span style={{ width: 200 }}>Principal</span>
              <span style={{ width: 60 }}>User ID</span>
              <span className="pos">Actions</span>
            </div>
            {list.map((l) => (
              <div className="qrow" key={`${l.provider}:${l.principal}`}>
                <span style={{ width: 150, fontSize: 12 }}>{l.provider}</span>
                <span style={{ width: 200, fontFamily: "var(--mono)", fontSize: 11 }}>{l.principal}</span>
                <span style={{ width: 60, fontFamily: "var(--mono)", fontSize: 12 }}>{l.user_id}</span>
                <span className="pos">
                  {canAdmin && (
                    <ConfirmButton
                      pending={remove.isPending}
                      onConfirm={() => submitDelete(l.provider, l.principal)}
                      label="Unlink"
                      warning={`Unlink ${l.provider}:${l.principal}?`}
                    />
                  )}
                </span>
              </div>
            ))}
          </>
        )}
        {canAdmin && (
          creating ? (
            <div className="form-grid" style={{ marginTop: 14 }}>
              <label className="form-row">Provider
                <select value={draftProvider} onChange={(e) => setDraftProvider(e.target.value as AuthNetworkProvider)}>
                  {NETWORK_PROVIDERS.filter((p) => p !== "none").map((p) => <option key={p} value={p}>{p}</option>)}
                </select>
              </label>
              <label className="form-row">Principal
                <input value={draftPrincipal} placeholder="user@github" onChange={(e) => setDraftPrincipal(e.target.value)} />
              </label>
              <label className="form-row">User ID
                <input
                  type="number"
                  min={1}
                  value={draftUserID}
                  onChange={(e) => setDraftUserID(Number(e.target.value))}
                />
              </label>
              <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn" onClick={() => { setCreating(false); setDraftPrincipal(""); }}>Cancel</button>
                <button className="btn primary" disabled={create.isPending || !draftPrincipal} onClick={submitCreate}>Link</button>
              </div>
            </div>
          ) : (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setCreating(true); setError(null); }}>
              + Add link
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
      <div className="eyebrow" id="security-totp">TOTP · authenticator app</div>
      <div className="card">
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {enrollData ? (
          <div>
            <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 10 }}>
              Scan this secret in your authenticator app, then enter the 6-digit code it generates.
            </div>
            <div style={{ fontFamily: "var(--mono)", fontSize: 12, wordBreak: "break-all", marginBottom: 8, padding: 8, background: "var(--bg-2)", borderRadius: 8 }}>
              {enrollData.secret}
            </div>
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 12 }}>
              otpauth: <span style={{ fontFamily: "var(--mono)" }}>{enrollData.otpauth_uri}</span>
            </div>
            <div style={{ display: "flex", gap: 8, alignItems: "flex-end", flexWrap: "wrap" }}>
              <label className="form-row" style={{ flex: "1 1 200px" }}>Confirmation code
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
                <button className="btn" onClick={() => { setEnrollData(null); setConfirmCode(""); }}>Cancel</button>
                <button className="btn primary" disabled={confirm.isPending || !confirmCode} onClick={submitConfirm}>Confirm</button>
              </div>
            </div>
          </div>
        ) : (
          <div style={{ display: "flex", gap: 8 }}>
            <button className="btn" disabled={enroll.isPending} onClick={startEnroll}>
              {enroll.isPending ? "Enrolling…" : "Enroll TOTP"}
            </button>
            <ConfirmButton
              pending={remove.isPending}
              onConfirm={submitDelete}
              label="Remove TOTP"
              warning="Remove TOTP? This disables 2FA for your account."
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
      <div className="eyebrow" id="security-webauthn">Passkeys · WebAuthn (L2)</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Phishing-resistant hardware/biometric credentials. A passkey registered under one RP ID
          does not work under another — re-enroll when the domain changes.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {list.length === 0 && !creds.isLoading && <div className="empty-note">No passkeys registered.</div>}
        {list.length > 0 && (
          <>
            <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
              <span style={{ width: 200 }}>Credential ID</span>
              <span style={{ width: 100 }}>Label</span>
              <span style={{ width: 100 }}>Last used</span>
              <span className="pos">Actions</span>
            </div>
            {list.map((c) => (
              <div className="qrow" key={c.id}>
                <span style={{ width: 200, fontFamily: "var(--mono)", fontSize: 10 }}>{c.id.slice(0, 24)}…</span>
                <span style={{ width: 100, fontSize: 12 }}>{c.label || "—"}</span>
                <span style={{ width: 100, fontSize: 12, color: "var(--text-dim)" }}>
                  {c.last_used_at ? new Date(c.last_used_at * 1000).toLocaleDateString() : "never"}
                </span>
                <span className="pos">
                  <ConfirmButton pending={remove.isPending} onConfirm={() => submitDelete(c.id)} label="Remove" warning="Remove this passkey?" />
                </span>
              </div>
            ))}
          </>
        )}
        <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
          <button className="btn" disabled={register.isPending} onClick={startRegister}>
            {register.isPending ? "Registering…" : "+ Register passkey"}
          </button>
          <button className="btn" disabled={assert.isPending} onClick={startAssert}>
            {assert.isPending ? "Verifying…" : "Step up with passkey"}
          </button>
        </div>
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

// ── Recovery codes ──────────────────────────────────────────────────────────

function RecoveryCodes() {
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
      <div className="eyebrow" id="security-recovery-codes">Recovery codes</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          One-time codes for regaining access if a TOTP device or passkey is lost. Store them safely —
          each code can be used once. Generating new codes replaces all existing ones.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {status.data && (
          <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12 }}>
            {status.data.has_codes
              ? `${status.data.unused} of ${status.data.total} codes unused.`
              : "No recovery codes generated yet."}
          </div>
        )}
        {codes && (
          <div className="card" style={{ background: "var(--bg-2)", marginBottom: 12, padding: 12 }}>
            <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 6 }}>Save these now — shown once:</div>
            <div style={{ fontFamily: "var(--mono)", fontSize: 12, lineHeight: 1.8 }}>
              {codes.map((c) => <div key={c}>{c}</div>)}
            </div>
            <div style={{ marginTop: 8 }}>
              <CopyButton text={codes.join("\n")} label="Copy all" />
              <button className="btn" style={{ marginLeft: 8 }} onClick={() => setCodes(null)}>Dismiss</button>
            </div>
          </div>
        )}
        <button className="btn" disabled={generate.isPending} onClick={submitGenerate}>
          {generate.isPending ? "Generating…" : codes ? "Regenerate codes" : "Generate codes"}
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
