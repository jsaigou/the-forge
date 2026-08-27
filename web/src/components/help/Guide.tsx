import { useState, type ReactNode } from "react";

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

type SectionKey = "orientation" | "running" | "scheduling" | "models" | "voice" | "cost" | "agent" | "security";

const SECTIONS: { key: SectionKey; label: string }[] = [
  { key: "orientation", label: "Orientation" },
  { key: "running", label: "Running models" },
  { key: "scheduling", label: "Scheduling & reservations" },
  { key: "models", label: "Models & configs" },
  { key: "voice", label: "Voice & speech" },
  { key: "cost", label: "Cost, power & Compressor" },
  { key: "agent", label: "Connecting an agent (a0)" },
  { key: "security", label: "Providers & security" },
];

function H({ children }: { children: string }) {
  return <h3><span className="tick" />{children}</h3>;
}

function P({ children }: { children: ReactNode }) {
  return <p style={{ fontSize: 12.5, color: "var(--text-dim)", lineHeight: 1.65, marginBottom: 12 }}>{children}</p>;
}

function OrientationSection() {
  return (
    <div className="card">
      <H>What this is</H>
      <P>
        The Forge runs local models on this box's own GPU and routes to remote providers
        through the same interface, so both look the same to whatever's asking for a completion.
      </P>
      <P>
        <b>Console</b> is the operational view: what's loaded right now, in four load bays
        (A1–A4), plus provider and service health. <b>Dashboard</b> covers cost, power, and
        Compressor savings across three tabs (Overview, Cost, Resources). <b>Models</b> is the full
        catalog: every model, its variants, and the local configs and remote offerings that
        serve them. <b>Scheduling</b> covers the on-demand loader's tunables and reservations.
        <b> Settings</b> is a left-sidebar shell grouped by what it changes: General; Money
        (Providers, Billing); Traffic (Routing &amp; Compressor); Workload (Scheduling, Monitoring,
        smith); Catalog (the catalog editor, Benchmarks &amp; Profiling); Access (Security); and
        Danger Zone.
      </P>
      <P>
        A <b>model</b> is the underlying weights (one entry per model on the Models page). A
        <b> variant</b> is a specific derivation of it (a quant, a fine-tune). A <b>config</b> is
        a concrete local launch recipe for a variant (context length, backend, cache quant,
        every llama.cpp flag) and what actually gets loaded into a bay. An <b>offering</b> is
        the remote equivalent: a model served by a specific provider at a specific price.
      </P>
    </div>
  );
}

function RunningSection() {
  return (
    <div className="card">
      <H>Load bays</H>
      <P>
        Four bays, A1–A4. Console shows only the bays that are actually in use (loaded, loading,
        unloading, or reserved) plus the first free one, so an idle box shows just A1 with a
        "+ Load model" button rather than four empty slots. The scheduler places a load on
        whichever bay is free, never a fixed one, so nothing outside this app should ever
        assume a model lives on a specific bay.
      </P>
      <P>
        Loading a model can be refused. The most common reason is the fit check: the scheduler
        won't load something it doesn't have enough VRAM/GTT compressor for, given what's already
        loaded in the other bays. If a load is refused, check what else is running before
        assuming something is broken.
      </P>
      <H>Ejecting</H>
      <P>
        Eject frees a bay immediately. The scheduler will also idle-unload a model on its own
        after the configured idle window (Scheduling → Scheduler tunables) if nothing has used it.
      </P>
    </div>
  );
}

function SchedulingSection() {
  return (
    <div className="card">
      <H>On-demand loading</H>
      <P>
        Nothing needs to be manually loaded before use: the scheduler loads a model on first
        request and evicts it later based on the tunables on the Scheduling page: idle-unload
        time, a small-job token threshold, and a priority-jump cap that lets a small, fast
        request cut ahead of a long-running one already queued.
      </P>
      <H>Reservations</H>
      <P>
        A reservation guarantees a bay for a model at (or from) a specific time, useful for
        anything that can't tolerate a cold-load delay, including scheduled ComfyUI jobs. The
        Scheduling page shows upcoming reservations and lets you cancel one.
      </P>
    </div>
  );
}

function ModelsSection() {
  return (
    <div className="card">
      <H>Adding a model from HuggingFace</H>
      <P>
        Models → <b>Add Model</b> searches HuggingFace directly, ranks a repo's real GGUF files
        against this host's live memory budget, and runs pre-flight checks (disk headroom, which
        backend the file needs, an existing-file conflict) before you commit to anything.
      </P>
      <P>
        Clicking Download starts a real, resumable transfer: pause and resume pick up from where
        they left off rather than restarting, and a repo split into shards (…-00001-of-00003.gguf
        and friends) downloads as one job. Progress, speed, and ETA show live in the Downloads
        list at the top of that tab.
      </P>
      <P>
        Once verified, the model registers itself into the catalog automatically: a new Model,
        Variant, Artifact, and Config, with sensible defaults (<code>--parallel 1</code>, the
        model's own trained context, the right backend for its size). The new Config lands{" "}
        <b>unverified and hidden</b> on purpose: it won't appear in the dashboard switcher until
        you open it and promote it, the same review step every other catalog entry goes through.
      </P>
      <P>
        Gated repos (license click-through models like Gemma or Llama) need a HuggingFace access
        token first; set one in Settings → Security. A repo you have a specific artifact for
        already can also be pointed at an <i>existing</i> config instead of registering a new one,
        by supplying that config's name when starting the download via the API or through smith.
      </P>
      <H>Editing in place</H>
      <P>
        Opening a config or model card gives you an expanded detail view; its Edit button opens
        a premium editor in the same place, no jump to a different tab. Every field is
        available, including the structural ones (backend, weight artifact, mmproj).
      </P>
      <H>The flag picker</H>
      <P>
        llama.cpp launch flags are tap-to-select rather than freeform text, with an explanation
        of what each one does, and for two flags, an inline hazard warning:
      </P>
      <P>
        <b>--parallel</b> above 1 splits the config's own context across that many slots rather
        than multiplying it, so a model configured for 128K context with <code>--parallel 2</code>{" "}
        actually serves 64K per request. Anything meant to serve one long conversation at its
        full configured context needs <code>--parallel 1</code>.
      </P>
      <P>
        <b>--ctx-checkpoints</b> set to a non-zero value on certain model architectures can hit an
        upstream llama.cpp hang bug. The picker warns inline rather than silently letting it
        through; if you're unsure whether a specific model is affected, leave it at 0.
      </P>
      <P>
        Anything not in the curated list still works: it falls into an Advanced section as a raw
        flag/value pair, or the freeform escape hatch. Every save can carry an optional
        "why this change" note, visible afterward in the config or model's own change history.
      </P>
      <H>Notes</H>
      <P>
        Below Key features on a model or config's detail view, Notes is a place for operator
        judgment that isn't a curated benchmark or a change-history entry: "tends to truncate
        long outputs," "needs the reasoning-fix build," anything the next person looking at this
        model should know. Admins can add, edit, and delete inline; everyone can read.
      </P>
      <H>Benchmarks &amp; Profiling</H>
      <P>
        Settings → Benchmarks &amp; Profiling groups curated performance/capability scores by
        config: a config's own benchmarks first, then what it inherits from its variant and
        model (a scope chip on each row always names which one it actually came from), next to
        that config's own measured profile if one exists. Profiling is the destructive part:
        "Profile…" evicts whatever else is loaded to measure one config alone: memory footprint
        and prefill/decode throughput at four context depths (empty, 25%, 50%, ~full), and
        always shows exactly which bays will be evicted before you confirm.
      </P>
    </div>
  );
}

function VoiceSection() {
  return (
    <div className="card">
      <H>Speech services</H>
      <P>
        Settings → Voice &amp; Speech's <b>Speech services</b> card starts and stops STT,
        Embedding, and Aligner independently. A blank/"Not configured" chip means the service
        genuinely has no dedicated resident unit on this host, not a loading glitch.
      </P>
      <H>Voice engines</H>
      <P>
        The <b>Voice engines</b> card covers TTS's four engines, one compact row each: Kokoro
        (fast tier), Custom voice, Voice design, and Base/clone. Each row toggles the engine on
        or off and picks its mode; "Details" expands a row to its unit/URL fields, which are
        rarely touched.
      </P>
      <H>Listing voices</H>
      <P>
        <b>List all voices</b>, on the Voice engines card, opens every registered voice grouped
        by the engine that serves it, with a built-in/custom marker and language per voice. New
        voices themselves (recording or cloning a reference, design prompts) are created on the
        host through forge-tts's own tooling, not through this app.
      </P>
    </div>
  );
}

function CostSection() {
  return (
    <div className="card">
      <H>What's measured vs. estimated</H>
      <P>
        Dashboard → Overview shows the box's real, measured electricity draw (a live power
        sensor), not an estimate. Dashboard → Cost separates that real electricity cost from
        <b> virtual spend</b> (what local inference would have cost at a comparable API's prices,
        a counterfactual, not a bill) and <b>real API spend</b> (what remote-provider requests
        through a0 actually cost, tracked from each response's own usage data). These are three
        different numbers and are deliberately never summed into one "total cost": electricity
        and virtual spend are two different estimates of the same local draw, not additive costs.
      </P>
      <H>Compressor savings</H>
      <P>
        Compressor compresses context before it reaches a model. Its savings show as two separate,
        honestly-labeled figures rather than one blended number: <b>local</b> savings (time saved
        by not re-prefilling context that was already cached, estimated from measured
        throughput) and <b>external</b> savings (money saved from Compressor's own context
        compression on remote requests, priced at the window's own blended input rate). The
        external figure is deliberately <i>not</i> the provider's own prompt-cache discount;
        that applies whether or not Compressor is in the request path at all, so crediting it to
        Compressor would overstate what Compressor is doing. The provider's own discount is real too;
        it's shown separately, on each proxy's own card in the Cost tab, labeled as the provider's
        discount rather than a Compressor saving.
      </P>
    </div>
  );
}

function AgentSection() {
  return (
    <div className="card">
      <H>The a0 router</H>
      <P>
        a0 is an OpenAI-compatible endpoint that fronts both local load bays and remote
        providers, so an external agent talks to one address regardless of where a given model
        actually runs. It's the integration point for anything outside this app: OpenCode,
        LibreChat, or a custom client.
      </P>
      <H>Getting a key</H>
      <P>
        Settings → Security → API keys mints a bearer key for a machine consumer; pick kind
        <code> router</code> for an a0 consumer (OpenCode, LibreChat, a custom client) and give it
        a name. The token is shown once, at creation time; copy it immediately, since the server
        never echoes it back afterward. Revoke a key from the same list. Use the model names shown
        on the Models page as the wire model name in requests.
      </P>
      <H>MCP</H>
      <P>
        A separate MCP server exposes scheduler control (status, load, evict) as tools for an
        agent that needs to manage its own inference resources rather than just call them. Mint
        an MCP-kind key the same way, from Settings → Security → API keys, kind <code>mcp</code>.
      </P>
    </div>
  );
}

function SecuritySection() {
  return (
    <div className="card">
      <H>Providers</H>
      <P>
        Settings → Providers links a remote provider (a curated preset prefills the endpoint,
        billing API, and currency where one exists) and stores its API key write-only, once
        saved, the key is never echoed back by the server, only a masked form. A provider can be
        renamed in place, and its page shows the actual models it serves (built from real
        offerings, not a static count) rather than just a chip.
      </P>
      <H>Assurance levels &amp; step-up</H>
      <P>
        Sensitive actions (provider keys, security settings, load/unload, reservations, Compressor
        teardown) require a minimum assurance level. Network identity and a valid password get
        you the base level; a step-up prompt (password, TOTP, or a passkey) raises it temporarily
        when an action needs more than you currently have, and expires after the configured
        step-up TTL.
      </P>
      <H>TOTP, passkeys, recovery codes</H>
      <P>
        Settings → Security lets you enroll a TOTP authenticator app, register a WebAuthn
        passkey (hardware key or biometric, the strongest factor), and generate recovery codes
        for account recovery if you lose access to both. Recovery codes are shown once at
        generation time; store them somewhere safe.
      </P>
    </div>
  );
}

export function Guide() {
  const [section, setSection] = useState<SectionKey>("orientation");
  const Active =
    section === "orientation" ? OrientationSection :
    section === "running" ? RunningSection :
    section === "scheduling" ? SchedulingSection :
    section === "models" ? ModelsSection :
    section === "voice" ? VoiceSection :
    section === "cost" ? CostSection :
    section === "agent" ? AgentSection :
    SecuritySection;

  return (
    <>
      <div className="tabs" style={{ marginBottom: 14, display: "inline-flex", flexWrap: "wrap" }}>
        {SECTIONS.map((s) => (
          <button
            key={s.key}
            className={`tab ${section === s.key ? "active" : ""}`}
            onClick={() => setSection(s.key)}
          >
            {s.label}
          </button>
        ))}
      </div>
      <Active />
    </>
  );
}
