import { useState, type ReactNode, type JSX } from "react";
import { Trans, useTranslation } from "react-i18next";
import type { TFunction } from "i18next";

// Sprint F: an operator usage guide reachable from inside the app itself.
// Everything below is written against this app's actual current behavior
// (checked live, not copied from docs, several of which CLAUDE.md itself
// flags as stale), and deliberately covers app usage + the a0/API consumer
// guide only. Host-level runbook material (SSH, deploy, systemd, per-slot
// sysconfig env files) stays in docs/deployment.md / docs/pitfalls.md / the
// vault, where an operator with a repo checkout already looks for it; a
// copy here would just be a second copy to keep in sync.
//
// Phase 8 (pre-release feedback sprint, 2026-08-13) audited every claim in
// every section against the live UI rather than just the two items flagged
// by the operator, and found a real, flatly false one: the Agent section
// claimed there was no self-service key creation for a0/MCP consumers, when
// Settings → Security → API keys does exactly that (and has since the
// FE-AUTH sprint). Also corrected: Dashboard tab names (Phase 5 renamed
// Trends → Resources), the Settings section list (predates the Sprint 12
// sidebar shell), and the Compressor external-savings description (it prices
// Compressor's own compression counter, not the provider's cache discount;
// fixed 2026-07-31, this section still described the pre-fix behavior).
// Added: notes on model/config detail views and the merged Benchmarks &
// Profiling section (both this phase), and provider rename (Phase 7).
//
// v0.5 feedback Sprint 5 (2026-08-27) added the Voice & speech section below,
// covering Settings → Voice & Speech (built Tier 1 Sprint 2) and the "list
// voices by engine" modal (built Sprint 1 of this same feedback series);
// this guide previously had zero mentions of voice/TTS despite both existing.
//
// i18n (Phase 1 step 8): every string below moved into locales/{en,ja}/
// help.json under "guide.*", one sub-key per <H>/<P> pair, organized by
// section heading (see GLOSSARY.md for the Japanese terminology). Tagged
// paragraphs use <Trans> with named components (b/code/i) rather than
// positional tags — react-i18next substitutes every occurrence of a given
// tag name regardless of count, so one components map covers a paragraph
// with several <b> spans.

type SectionKey = "orientation" | "running" | "scheduling" | "models" | "voice" | "cost" | "agent" | "security";

const SECTION_KEYS: SectionKey[] = ["orientation", "running", "scheduling", "models", "voice", "cost", "agent", "security"];

function H({ children }: { children: string }) {
  return <h3><span className="tick" />{children}</h3>;
}

function P({ children }: { children: ReactNode }) {
  return <p style={{ fontSize: 12.5, color: "var(--text-dim)", lineHeight: 1.65, marginBottom: 12 }}>{children}</p>;
}

const TAGS = { b: <b />, code: <code />, i: <i /> };

function OrientationSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.orientation.what_this_is.heading")}</H>
      <P>{t("guide.orientation.what_this_is.p1")}</P>
      <P><Trans i18nKey="guide.orientation.what_this_is.p2" ns="help" components={TAGS} /></P>
      <P><Trans i18nKey="guide.orientation.what_this_is.p3" ns="help" components={TAGS} /></P>
    </div>
  );
}

function RunningSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.running.load_bays.heading")}</H>
      <P>{t("guide.running.load_bays.p1")}</P>
      <P>{t("guide.running.load_bays.p2")}</P>
      <H>{t("guide.running.ejecting.heading")}</H>
      <P>{t("guide.running.ejecting.p1")}</P>
    </div>
  );
}

function SchedulingSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.scheduling.on_demand.heading")}</H>
      <P>{t("guide.scheduling.on_demand.p1")}</P>
      <H>{t("guide.scheduling.reservations.heading")}</H>
      <P>{t("guide.scheduling.reservations.p1")}</P>
    </div>
  );
}

function ModelsSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.models.adding_hf.heading")}</H>
      <P><Trans i18nKey="guide.models.adding_hf.p1" ns="help" components={TAGS} /></P>
      <P>{t("guide.models.adding_hf.p2")}</P>
      <P><Trans i18nKey="guide.models.adding_hf.p3" ns="help" components={TAGS} /></P>
      <P><Trans i18nKey="guide.models.adding_hf.p4" ns="help" components={TAGS} /></P>
      <H>{t("guide.models.editing.heading")}</H>
      <P>{t("guide.models.editing.p1")}</P>
      <H>{t("guide.models.flag_picker.heading")}</H>
      <P>{t("guide.models.flag_picker.p1")}</P>
      <P><Trans i18nKey="guide.models.flag_picker.p2" ns="help" components={TAGS} /></P>
      <P><Trans i18nKey="guide.models.flag_picker.p3" ns="help" components={TAGS} /></P>
      <P>{t("guide.models.flag_picker.p4")}</P>
      <H>{t("guide.models.notes.heading")}</H>
      <P>{t("guide.models.notes.p1")}</P>
      <H>{t("guide.models.benchmarks.heading")}</H>
      <P>{t("guide.models.benchmarks.p1")}</P>
    </div>
  );
}

function VoiceSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.voice.speech_services.heading")}</H>
      <P><Trans i18nKey="guide.voice.speech_services.p1" ns="help" components={TAGS} /></P>
      <H>{t("guide.voice.voice_engines.heading")}</H>
      <P><Trans i18nKey="guide.voice.voice_engines.p1" ns="help" components={TAGS} /></P>
      <H>{t("guide.voice.listing_voices.heading")}</H>
      <P><Trans i18nKey="guide.voice.listing_voices.p1" ns="help" components={TAGS} /></P>
    </div>
  );
}

function CostSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.cost.measured_vs_estimated.heading")}</H>
      <P><Trans i18nKey="guide.cost.measured_vs_estimated.p1" ns="help" components={TAGS} /></P>
      <H>{t("guide.cost.compressor_savings.heading")}</H>
      <P><Trans i18nKey="guide.cost.compressor_savings.p1" ns="help" components={TAGS} /></P>
    </div>
  );
}

function AgentSection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.agent.a0_router.heading")}</H>
      <P>{t("guide.agent.a0_router.p1")}</P>
      <H>{t("guide.agent.getting_a_key.heading")}</H>
      <P><Trans i18nKey="guide.agent.getting_a_key.p1" ns="help" components={TAGS} /></P>
      <H>{t("guide.agent.mcp.heading")}</H>
      <P><Trans i18nKey="guide.agent.mcp.p1" ns="help" components={TAGS} /></P>
    </div>
  );
}

function SecuritySection({ t }: { t: TFunction }) {
  return (
    <div className="card">
      <H>{t("guide.security.providers.heading")}</H>
      <P>{t("guide.security.providers.p1")}</P>
      <H>{t("guide.security.assurance.heading")}</H>
      <P>{t("guide.security.assurance.p1")}</P>
      <H>{t("guide.security.totp.heading")}</H>
      <P>{t("guide.security.totp.p1")}</P>
    </div>
  );
}

const SECTION_COMPONENTS: Record<SectionKey, (props: { t: TFunction }) => JSX.Element> = {
  orientation: OrientationSection,
  running: RunningSection,
  scheduling: SchedulingSection,
  models: ModelsSection,
  voice: VoiceSection,
  cost: CostSection,
  agent: AgentSection,
  security: SecuritySection,
};

export function Guide() {
  const { t } = useTranslation("help");
  const [section, setSection] = useState<SectionKey>("orientation");
  const Active = SECTION_COMPONENTS[section];

  return (
    <>
      <div className="tabs" style={{ marginBottom: 14, display: "inline-flex", flexWrap: "wrap" }}>
        {SECTION_KEYS.map((key) => (
          <button
            key={key}
            className={`tab ${section === key ? "active" : ""}`}
            onClick={() => setSection(key)}
          >
            {t(`guide.tabs.${key}`)}
          </button>
        ))}
      </div>
      <Active t={t} />
    </>
  );
}
