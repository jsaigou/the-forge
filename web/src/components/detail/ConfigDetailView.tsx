import { formatGB, formatCurrency } from "../../lib/format";
import { hazardsFor, parseLoadOptions, VLLM_FLAGS } from "../../lib/llamaFlags";
import { findProfileForConfig } from "../../lib/profileJoin";
import { useProfiles } from "../../lib/queries";
import { useSession } from "../../lib/session";
import { useLoadConfig } from "../../lib/useLoadConfig";
import type { ConfigCard, SchedulerStatus, Status } from "../../lib/types";
import { BadgeIcon } from "../BadgeIcon";
import { CapabilityBar } from "../CapabilityBar";
import { ChangeHistory } from "../ChangeHistory";
import { CopyButton } from "../CopyButton";
import { Icon } from "../Icon";
import { InfoTip } from "../InfoTip";
import { LoadConfirmModal } from "../LoadConfirmModal";
import { NotesInline } from "./NotesInline";

// ConfigDetailView — the redesigned config expanded view (Sprint B):
// 128×128 logo, a justified spec table (replacing the old flat
// `<b>label</b> value` prose run), the load-options list (the config's real
// llama.cpp flags, each with curated "why this exists" hover text from
// lib/llamaFlags.ts), and an edit-in-place button styled like Load. Edit
// (Sprint C) pushes ConfigEditView into DetailModal's own view stack via
// `onEdit` rather than stacking a second modal-backdrop over this one —
// see DetailModal.tsx.
export function ConfigDetailView({
  card,
  status,
  schedulerStatus,
  displayCurrency = "USD",
  onViewModel,
  onEdit,
}: {
  card: ConfigCard;
  status: Status;
  schedulerStatus: SchedulerStatus | undefined;
  displayCurrency?: string;
  onViewModel: (modelId: string) => void;
  onEdit: () => void;
}) {
  const { canOperate, canAdmin } = useSession();
  const loadState = useLoadConfig(card, status);
  const { state, activeSlot, busy, openConfirm, confirming } = loadState;

  const { data: profilesResp } = useProfiles();
  const profile = findProfileForConfig(profilesResp?.profiles ?? [], card);
  const hasFreshProfile = !!profile && !profile.stale;
  const memReqBytes = hasFreshProfile ? profile.safe_memory_bytes : card.derived.memory_req_bytes;
  const fits =
    memReqBytes != null && schedulerStatus
      ? memReqBytes <= schedulerStatus.memory_budget.free_bytes
      : null;
  const powerEstPer1m =
    card.performance.power_est_per_1m != null
      ? card.performance.power_est_per_1m
      : card.performance.power_cost_per_1k > 0
        ? card.performance.power_cost_per_1k * 1000
        : null;

  // carbon-8b/hy-mt2 are vLLM, not llama.cpp (ConfigCard.backend) — offering
  // llama.cpp's flag explanations for those would be actively wrong, so
  // this looks up VLLM_FLAGS instead for those two.
  const loadOptions = parseLoadOptions(card.extra_args, card.backend === "vllm" ? VLLM_FLAGS : undefined);
  const hazards = hazardsFor(card);
  const hazardByFlag = new Map(hazards.map((h) => [h.flag, h.message]));

  return (
    <div className="detail-view">
      <div className="detail-head">
        <Icon slug={card.logo} name={card.model_name} xl />
        <div style={{ flex: "1 1 auto", minWidth: 0 }}>
          <h3 style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 2 }}>
            {card.name}
            <CopyButton text={card.name} title="Copy config name" />
            {card.badges.length > 0 && (
              <span className="mbadges">
                {card.badges.map((b) => (
                  <BadgeIcon key={b.id} badge={b} />
                ))}
              </span>
            )}
          </h3>
          <div className="mmaker">{[card.creator, card.license_name, card.family].filter(Boolean).join(" · ")}</div>
        </div>
        <button
          className="chip"
          style={{ cursor: "pointer", border: "1px solid var(--border)", background: "var(--panel-2)" }}
          onClick={() => onViewModel(card.model_id)}
        >
          View model →
        </button>
      </div>

      {card.description && <div className="mdesc" style={{ WebkitLineClamp: "unset", minHeight: 0 }}>{card.description}</div>}

      <div className="spec-table">
        <div className="spec-row"><span className="k">Model</span><span className="v">{card.model_name}</span></div>
        {card.variant_name && <div className="spec-row"><span className="k">Variant</span><span className="v">{card.variant_name}</span></div>}
        {card.backend && <div className="spec-row"><span className="k">Backend</span><span className="v">{card.backend}</span></div>}
        {card.derived.arch && <div className="spec-row"><span className="k">Architecture</span><span className="v">{card.derived.arch}</span></div>}
        {(card.modalities.length > 0 || card.modalities_unavailable.length > 0) && (
          <div className="spec-row">
            <span className="k">Modalities</span>
            <span className="v">
              {card.modalities.map((m) => m[0].toUpperCase() + m.slice(1)).join(", ")}
              {card.modalities_unavailable.map((g) => (
                <InfoTip key={g.id} text={g.reason}>
                  <span style={{ textDecoration: "line-through", opacity: 0.5, marginLeft: 8, cursor: "help" }}>
                    {g.id[0].toUpperCase() + g.id.slice(1)}
                  </span>
                </InfoTip>
              ))}
            </span>
          </div>
        )}
        <div className="spec-row"><span className="k">Configured context</span><span className="v">{card.n_ctx.toLocaleString()}</span></div>
        {card.derived.trained_ctx != null && (
          <div className="spec-row"><span className="k">Trained context</span><span className="v">{card.derived.trained_ctx.toLocaleString()}</span></div>
        )}
        {card.derived.file_size_bytes != null && (
          <div className="spec-row"><span className="k">File size</span><span className="v">{formatGB(card.derived.file_size_bytes)} GB</span></div>
        )}
        <div className="spec-row">
          <span className="k">Memory</span>
          <span className="v">
            {memReqBytes != null ? `${formatGB(memReqBytes)} GB` : "—"}
          </span>
        </div>
        <div className="spec-row">
          <span className="k">Throughput</span>
          <span className="v">{hasFreshProfile ? `${profile.decode_tps.toFixed(1)} T/s` : "—"}</span>
        </div>
        {powerEstPer1m != null && powerEstPer1m > 0 && (
          <div className="spec-row"><span className="k">Power est /1M</span><span className="v">~{formatCurrency(powerEstPer1m, displayCurrency)}</span></div>
        )}
        {card.license_name && (
          <div className="spec-row">
            <span className="k">License</span>
            <span className="v">
              {card.license_url ? <a href={card.license_url} target="_blank" rel="noreferrer">{card.license_name}</a> : card.license_name}
            </span>
          </div>
        )}
        {card.hf_repo && (
          <div className="spec-row">
            <span className="k">HF repo</span>
            <span className="v"><a href={`https://huggingface.co/${card.hf_repo}`} target="_blank" rel="noreferrer">{card.hf_repo}</a></span>
          </div>
        )}
        <div className="spec-row">
          <span className="k">Status</span>
          <span className="v">{card.status} · {card.visibility}{card.is_default ? " · default" : ""}</span>
        </div>
        {card.derived.history && (
          <div className="spec-row">
            <span className="k">Last load</span>
            <span className="v">
              {card.derived.history.last_result ?? "—"}
              {card.derived.history.avg_load_time_s != null && ` · avg ${card.derived.history.avg_load_time_s.toFixed(1)}s`}
            </span>
          </div>
        )}
        {card.derived.reliability && (
          <div className="spec-row">
            <span className="k">Reliability</span>
            <span className="v">
              {card.derived.reliability.loads_ok} ok · {card.derived.reliability.load_failures} failed
              {card.derived.reliability.inference_hangs > 0 && ` · ${card.derived.reliability.inference_hangs} hangs`}
            </span>
          </div>
        )}
      </div>

      {loadOptions.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>
            Load options
            <CopyButton text={card.extra_args.join(" ")} title="Copy full llama.cpp argument list" />
          </div>
          <div className="load-opts">
            {loadOptions.map((opt, i) => {
              const hazard = hazardByFlag.get(opt.flag);
              return (
                <div className={`load-opt ${hazard ? "hazard" : ""}`} key={`${opt.flag}-${i}`}>
                  <span className="flag">{opt.flag}</span>
                  <span className="val">{opt.value ?? <span className="unprofiled">on</span>}</span>
                  {opt.ref && <InfoTip text={opt.ref.why} />}
                  {hazard && <InfoTip text={hazard} className="hazard-tip" />}
                  {opt.malformed && (
                    <InfoTip
                      text="This flag was saved as a single malformed token (the ConfigForm's extra-args textarea used to teach this by example) — the launcher reads one argv token per line, so this config will fail to start until it's split into two lines."
                      className="hazard-tip"
                    />
                  )}
                </div>
              );
            })}
          </div>
        </div>
      )}

      {card.capabilities.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>Capabilities</div>
          <div className="caps" style={{ marginTop: 6 }}>
            {card.capabilities.map((cap) => <CapabilityBar key={cap.id} cap={cap} />)}
          </div>
        </div>
      )}

      {card.key_features.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>Key features</div>
          <ul style={{ margin: "6px 0 0 18px", padding: 0 }}>
            {card.key_features.map((f) => <li key={f}>{f}</li>)}
          </ul>
        </div>
      )}

      <NotesInline subjectType="config" subjectId={card.id} />

      {canAdmin && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>Change history</div>
          <ChangeHistory actionPrefix="catalog_config_" target={String(card.id)} />
        </div>
      )}

      <div className="form-actions" style={{ marginTop: 20, justifyContent: "flex-start" }}>
        {/* Sprint B: edit-in-place button visually matches Load (.go —
            the same heat-gradient treatment), distinguished only by label,
            not by a lesser style — editing is not a lesser action here. */}
        {canAdmin && (
          <button className="go" onClick={onEdit}>
            Edit
          </button>
        )}
        {canOperate && (
          state === "loaded" ? (
            <button className="go loaded" disabled>
              Loaded{activeSlot ? ` · ${status.slot_labels[activeSlot]}` : ""}
            </button>
          ) : state === "loading" ? (
            <button className="go loaded loading-glare" disabled>Loading…</button>
          ) : (
            <button
              className={`go ${state === "evict-needed" && fits === false ? "wontfit" : ""}`}
              disabled={busy}
              onClick={openConfirm}
            >
              {state === "evict-needed" ? "Evict & load" : "Load"}
            </button>
          )
        )}
      </div>

      {confirming && <LoadConfirmModal card={card} status={status} loadState={loadState} />}
    </div>
  );
}
