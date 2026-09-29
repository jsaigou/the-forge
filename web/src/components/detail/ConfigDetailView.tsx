import { useTranslation } from "react-i18next";
import { capLabel, dedupedCapEntries } from "../../lib/chatTemplateCaps";
import { translatedFlagWhy } from "../../lib/flagI18n";
import { formatGB, formatCurrency, formatRelativeTime } from "../../lib/format";
import { translatedHazardsFor } from "../../lib/hazardLabels";
import { canonicalFlag, parseLoadOptions, VLLM_FLAGS } from "../../lib/llamaFlags";
import { findProfileForConfig } from "../../lib/profileJoin";
import { useCatalogConfigs, useCatalogModelAliases, useProfiles } from "../../lib/queries";
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
  const { t } = useTranslation("models");
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
  const isVllm = card.backend === "vllm";
  const loadOptions = parseLoadOptions(card.extra_args, isVllm ? VLLM_FLAGS : undefined);
  const hazards = translatedHazardsFor(t, card);
  const hazardByFlag = new Map(hazards.map((h) => [h.flag, h.translated]));
  const capEntries = dedupedCapEntries(card.chat_template_caps);
  const { data: modelAliases } = useCatalogModelAliases();
  const aliasedAs = (modelAliases ?? []).filter((a) => a.config_id === card.id);
  // ConfigCard.chat_template_caps is already the *effective* (probe merged
  // with override) value — chat_template_caps_override itself only lives on
  // the raw CatalogConfig record, so an active override is looked up there
  // to mark which capEntries rows are a correction rather than a live probe
  // result (2026-09-14 — the override previously had no visibility anywhere
  // in the UI at all, see ChatTemplateCapsOverrideEditor's doc comment).
  const { data: rawConfigs } = useCatalogConfigs();
  const rawConfig = rawConfigs?.find((c) => c.id === card.id);
  const overriddenLabels = new Set(Object.keys(rawConfig?.chat_template_caps_override ?? {}).map(capLabel));

  return (
    <div className="detail-view">
      <div className="detail-head">
        <Icon slug={card.logo} name={card.model_name} xl />
        <div style={{ flex: "1 1 auto", minWidth: 0 }}>
          <h3 style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 2 }}>
            {card.name}
            <CopyButton text={card.name} title={t("detail.copy_config_title")} />
            {card.badges.length > 0 && (
              <span className="mbadges">
                {card.badges.map((b) => (
                  <BadgeIcon key={b.id} badge={b} />
                ))}
              </span>
            )}
          </h3>
          {/* Operator feedback (2026-09-14): "Also known as" was a spec-row
              buried in the middle of the table — moved directly under the
              config name (same position as ConfigCardView/Bay/ConfigRow)
              and to full-brightness text instead of a spec-row's muted "k"
              label. */}
          {aliasedAs.length > 0 && (
            <div style={{ fontSize: 12, color: "var(--text)", marginBottom: 2 }}>
              {t("detail.alias_prefix", { names: aliasedAs.map((a) => a.name).join(", ") })}
              <InfoTip text={t("detail.alias_infotip")} />
            </div>
          )}
          <div className="mmaker">{[card.creator, card.license_name, card.family].filter(Boolean).join(" · ")}</div>
        </div>
        <button
          className="chip"
          style={{ cursor: "pointer", border: "1px solid var(--border)", background: "var(--panel-2)" }}
          onClick={() => onViewModel(card.model_id)}
        >
          {t("detail.view_model")}
        </button>
      </div>

      {card.description && <div className="mdesc" style={{ WebkitLineClamp: "unset", minHeight: 0 }}>{card.description}</div>}

      <div className="spec-table">
        <div className="spec-row"><span className="k">{t("spec.model")}</span><span className="v">{card.model_name}</span></div>
        {card.variant_name && <div className="spec-row"><span className="k">{t("spec.variant")}</span><span className="v">{card.variant_name}</span></div>}
        {card.backend && <div className="spec-row"><span className="k">{t("spec.backend")}</span><span className="v">{card.backend}</span></div>}
        {card.derived.arch && <div className="spec-row"><span className="k">{t("spec.architecture")}</span><span className="v">{card.derived.arch}</span></div>}
        {(card.modalities.length > 0 || card.modalities_unavailable.length > 0) && (
          <div className="spec-row">
            <span className="k">{t("spec.modalities")}</span>
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
        <div className="spec-row"><span className="k">{t("spec.configured_context")}</span><span className="v">{card.n_ctx.toLocaleString()}</span></div>
        {card.derived.trained_ctx != null && (
          <div className="spec-row"><span className="k">{t("spec.trained_context")}</span><span className="v">{card.derived.trained_ctx.toLocaleString()}</span></div>
        )}
        {card.derived.file_size_bytes != null && (
          <div className="spec-row"><span className="k">{t("spec.file_size")}</span><span className="v">{formatGB(card.derived.file_size_bytes)} GB</span></div>
        )}
        <div className="spec-row">
          <span className="k">{t("spec.memory")}</span>
          <span className="v">
            {memReqBytes != null ? `${formatGB(memReqBytes)} GB` : "—"}
          </span>
        </div>
        <div className="spec-row">
          <span className="k">{t("spec.throughput")}</span>
          <span className="v">{hasFreshProfile ? `${profile.decode_tps.toFixed(1)} T/s` : "—"}</span>
        </div>
        {powerEstPer1m != null && powerEstPer1m > 0 && (
          <div className="spec-row"><span className="k">{t("spec.power_est")}</span><span className="v">~{formatCurrency(powerEstPer1m, displayCurrency)}</span></div>
        )}
        {card.license_name && (
          <div className="spec-row">
            <span className="k">{t("spec.license")}</span>
            <span className="v">
              {card.license_url ? <a href={card.license_url} target="_blank" rel="noreferrer">{card.license_name}</a> : card.license_name}
            </span>
          </div>
        )}
        {card.hf_repo && (
          <div className="spec-row">
            <span className="k">{t("spec.hf_repo")}</span>
            <span className="v"><a href={`https://huggingface.co/${card.hf_repo}`} target="_blank" rel="noreferrer">{card.hf_repo}</a></span>
          </div>
        )}
        <div className="spec-row">
          <span className="k">{t("spec.status")}</span>
          <span className="v">{t("spec.status_value", { status: card.status, visibility: card.visibility })}{card.is_default ? t("spec.status_default_suffix") : ""}</span>
        </div>
        {card.reasoning_effort_default && (
          <div className="spec-row">
            <span className="k">{t("spec.default_reasoning")}</span>
            <span className="v">{card.reasoning_effort_default}</span>
          </div>
        )}
        {card.derived.history && (
          <div className="spec-row">
            <span className="k">{t("spec.last_load")}</span>
            <span className="v">
              {card.derived.history.last_result ?? "—"}
              {card.derived.history.avg_load_time_s != null && t("spec.last_load_avg_suffix", { avg: card.derived.history.avg_load_time_s.toFixed(1) })}
            </span>
          </div>
        )}
        {card.derived.reliability && (
          <div className="spec-row">
            <span className="k">{t("spec.reliability")}</span>
            <span className="v">
              {t("spec.reliability_value", { ok: card.derived.reliability.loads_ok, failed: card.derived.reliability.load_failures })}
              {card.derived.reliability.inference_hangs > 0 && t("spec.reliability_hangs_suffix", { hangs: card.derived.reliability.inference_hangs })}
            </span>
          </div>
        )}
      </div>

      {loadOptions.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>
            {t("detail.load_options")}
            <CopyButton text={card.extra_args.join(" ")} title={t("detail.copy_args_title")} />
          </div>
          <div className="load-opts">
            {loadOptions.map((opt, i) => {
              const hazard = hazardByFlag.get(opt.flag);
              return (
                <div className={`load-opt ${hazard ? "hazard" : ""}`} key={`${opt.flag}-${i}`}>
                  <span className="flag">{opt.flag}</span>
                  <span className="val">{opt.value ?? <span className="unprofiled">on</span>}</span>
                  {opt.ref && <InfoTip text={translatedFlagWhy(t, isVllm, canonicalFlag(opt.flag), opt.ref.why)} />}
                  {hazard && <InfoTip text={hazard} className="hazard-tip" />}
                  {opt.malformed && (
                    <InfoTip
                      text={t("detail.malformed_flag_warning")}
                      className="hazard-tip"
                    />
                  )}
                </div>
              );
            })}
          </div>
        </div>
      )}

      {capEntries.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>
            {t("detail.chat_template_caps")}
            <InfoTip
              text={
                card.chat_template_caps_probed_at > 0
                  ? t("detail.chat_template_caps_probed", { when: formatRelativeTime(card.chat_template_caps_probed_at) })
                  : t("detail.chat_template_caps_unprobed")
              }
            />
          </div>
          <div className="load-opts">
            {capEntries.map((entry) => (
              <div className="load-opt" key={entry.key}>
                <span className="flag">{t(entry.label, { defaultValue: entry.label })}</span>
                <span className="val">{entry.value ? "✓" : "✗"}</span>
                {overriddenLabels.has(entry.label) && (
                  <InfoTip text={t("detail.caps_override_note")} className="hazard-tip" />
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {card.capabilities.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>{t("detail.capabilities")}</div>
          <div className="caps" style={{ marginTop: 6 }}>
            {card.capabilities.map((cap) => <CapabilityBar key={cap.id} cap={cap} />)}
          </div>
        </div>
      )}

      {card.key_features.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>{t("detail.key_features")}</div>
          <ul style={{ margin: "6px 0 0 18px", padding: 0 }}>
            {card.key_features.map((f) => <li key={f}>{f}</li>)}
          </ul>
        </div>
      )}

      <NotesInline subjectType="config" subjectId={card.id} />

      {canAdmin && (
        <div style={{ marginTop: 16 }}>
          <div className="eyebrow" style={{ fontSize: 11 }}>{t("detail.change_history")}</div>
          <ChangeHistory actionPrefix="catalog_config_" target={String(card.id)} />
        </div>
      )}

      <div className="form-actions" style={{ marginTop: 20, justifyContent: "flex-start" }}>
        {/* Sprint B: edit-in-place button visually matches Load (.go —
            the same heat-gradient treatment), distinguished only by label,
            not by a lesser style — editing is not a lesser action here. */}
        {canAdmin && (
          <button className="go" onClick={onEdit}>
            {t("detail.edit")}
          </button>
        )}
        {canOperate && (
          state === "loaded" ? (
            <button className="go loaded" disabled>
              {t("detail.loaded")}{activeSlot ? t("detail.loaded_slot_suffix", { slot: status.slot_labels[activeSlot] }) : ""}
            </button>
          ) : state === "loading" ? (
            <button className="go loaded loading-glare" disabled>{t("detail.loading_state")}</button>
          ) : (
            <button
              className={`go ${state === "evict-needed" && fits === false ? "wontfit" : ""}`}
              disabled={busy}
              onClick={openConfirm}
            >
              {state === "evict-needed" ? t("detail.evict_and_load") : t("detail.load")}
            </button>
          )
        )}
      </div>

      {confirming && <LoadConfirmModal card={card} status={status} loadState={loadState} />}
    </div>
  );
}
