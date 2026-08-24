import type { ConfigCard } from "./types";

// Curated llama.cpp launch-flag reference, used by the config expanded
// view's "load options" list (Sprint B) and reused as-is by Sprint C's
// tap-to-select flag picker — write once, reuse (see
// docs/v5-prerelease-readiness.md's Sprint B/C entries). Modeled on
// benchmarks.ts's BENCHMARK_REFS: a small curated table + a documented
// fallback for anything not in it, rather than inventing an explanation at
// render time.
//
// `why` is operator-facing prose, not a flag-syntax reference (llama.cpp's
// own --help already covers syntax) — it should answer "why would this
// config carry this flag, and what happens if I remove it," grounded in
// this hardware's real, previously-hit failure modes where one exists
// (see CLAUDE.md's "Critical Hardware / Runtime Facts (ForgeHost)").
//
// Unknown flags render with no `ref` (a plain flag/value row, no tooltip)
// rather than a guessed explanation — re-verify/extend this table whenever
// a new flag shows up in a real config rather than leaving it unexplained
// forever.
export interface FlagRef {
  label: string;
  why: string;
  // Sprint C (LoadOptionsEditor): how the picker should render this flag's
  // value control. "none" = boolean tap-to-toggle chip (flag with no
  // argument token), "number" = numeric input, "text" = free text input,
  // optionally with `presets` quick-pick chips. `presets` is a curated
  // shortlist of values actually seen on this box, not an exhaustive enum —
  // llama.cpp accepts values beyond it, so the text input always stays
  // editable rather than becoming a rigid dropdown.
  arg: "none" | "number" | "text";
  presets?: string[];
  placeholder?: string;
  // managed=true means the picker must not offer this as an addable flag —
  // it's set some other way (right now, only --mmproj, from the config's
  // linked mmproj artifact). It still renders read-only with an
  // explanatory note if somehow present in extra_args.
  managed?: boolean;
}

export const LLAMA_FLAGS: Record<string, FlagRef> = {
  "--parallel": {
    label: "Parallel slots",
    why: "Splits the configured context N ways rather than multiplying it — n_ctx 131072 with --parallel 2 gives each request only 65536 usable tokens. Must be 1 for any mode meant to serve one long conversation at its full context; qwen25coder's --parallel 4 is the one deliberate exception (a short fast-worker mode for concurrent short requests). Found live 2026-07-23 on gemma4-e2b/nemotron-nano.",
    arg: "number",
  },
  "--ctx-checkpoints": {
    label: "Context checkpoints",
    why: "Lets the engine restore a saved KV-cache checkpoint instead of reprocessing a whole conversation from scratch. Upstream llama.cpp has open, unresolved hang bugs in this exact code path as of 2026-07 (docs/investigations.md item 9) — most configs on this box use 16, swallow-32b/swallow-8b explicitly use 0 to avoid it.",
    arg: "number",
  },
  "--cache-type-k": {
    label: "KV cache quant (K)",
    why: "Quantizes the attention key cache to shrink its memory footprint at some quality cost. Several configs already run this asymmetric with --cache-type-v (e.g. q4_0/q8_0) — a real, already-shipped tradeoff, not a hazard.",
    arg: "text",
    presets: ["q4_0", "q8_0", "f16"],
  },
  "--cache-type-v": {
    label: "KV cache quant (V)",
    why: "Quantizes the attention value cache. Paired with --cache-type-k; see that entry.",
    arg: "text",
    presets: ["q4_0", "q8_0", "f16"],
  },
  "--flash-attn": {
    label: "Flash attention",
    why: "Enables the fused flash-attention kernel — faster and lower-memory attention on supported backends. Safe to leave on; it's on for nearly every config on this box.",
    arg: "text",
    presets: ["on", "off", "auto"],
  },
  "--jinja": {
    label: "Jinja chat template",
    why: "Uses the model's own Jinja chat template (from the GGUF metadata) instead of llama.cpp's built-in template guesser. Needed for any model whose prompt format the built-in guesser doesn't already know.",
    arg: "none",
  },
  "--mmproj": {
    label: "Multimodal projector",
    why: "Path to the vision/multimodal projector weights, required for any model that accepts image input. Set automatically from the config's linked mmproj artifact, not typed by hand.",
    arg: "text",
    managed: true,
  },
  "--no-mmap": {
    label: "Disable mmap",
    why: "Forces the whole weight file to be read into memory up front instead of memory-mapped on demand. Slower to start, but avoids page-fault stalls mid-generation on some filesystem/GPU-driver combinations.",
    arg: "none",
  },
  "--threads": {
    label: "CPU threads",
    why: "CPU thread count for the parts of inference that aren't offloaded to the GPU (tokenization, sampling, any CPU-resident layers). Rarely the bottleneck on this box's GPU-bound modes.",
    arg: "number",
  },
  "--swa-full": {
    label: "Full sliding-window attention cache",
    why: "Keeps the full sliding-window attention cache resident rather than the reduced form — needed by some hybrid/recurrent architectures to reuse cache across turns. Interacts with --ctx-checkpoints; read docs/investigations.md item 9 before changing either on a model that sets this.",
    arg: "none",
  },
  "--spec-type": {
    label: "Speculative decoding",
    why: "Selects the speculative-decoding strategy (e.g. a draft model or MTP head) used to predict several tokens ahead and verify them in a batch — a real throughput win when the draft's acceptance rate is high, and a real slowdown when it isn't.",
    arg: "text",
    presets: ["draft-mtp", "draft-dflash", "ngram-map-k"],
  },
  "--spec-draft-model": {
    label: "Draft model",
    // NOT actually catalog-managed, unlike --mmproj — ground-truthed
    // against store.Config (go/internal/store/catalog.go) while planning
    // Sprint C: there is no DraftArtifactID/spec-draft artifact link
    // anywhere in the schema. It's only ever a hand-typed path in
    // extra_args today, same as its short-flag sibling --model-draft. A
    // prior version of this comment claimed otherwise — corrected here.
    why: "Path to the smaller draft model used for speculative decoding, hand-typed (there is no catalog-linked draft-model artifact, unlike --mmproj). qwen36-mtp's config sets --spec-type draft-mtp with no draft-model path at all — worth checking if that's intentional before copying it as a template.",
    arg: "text",
    placeholder: "/opt/forge/models/…",
  },
  "--spec-draft-n-max": {
    label: "Max draft tokens",
    why: "Upper bound on how many tokens the draft model predicts ahead before the main model verifies them in one batch. Larger values raise the potential speedup but also the cost of a rejected batch.",
    arg: "number",
  },
  "--model-draft": {
    label: "Draft model",
    why: "Short-flag form of the draft-model path (-md), used by this box's draft-dflash configs (laguna-s-21) where --spec-draft-model is used by draft-mtp configs instead — same role, different flag name depending on which speculative-decoding path the config uses. Hand-typed, not catalog-linked — see --spec-draft-model.",
    arg: "text",
    placeholder: "/opt/forge/models/…",
  },
  "--n-gpu-layers": {
    label: "GPU-offloaded layers",
    why: "Number of transformer layers placed on the GPU rather than the CPU. 999 (nemotron-puzzle's value) means \"all of them\" — llama.cpp clamps to the model's real layer count. Lowering this trades inference speed for less GPU memory pressure.",
    arg: "number",
  },
  "--batch-size": {
    label: "Prompt batch size",
    why: "Logical batch size for prompt (prefill) processing. Larger values process a prompt faster at the cost of a bigger compute buffer; several configs on this box pair it with --ubatch-size.",
    arg: "number",
  },
  "--ubatch-size": {
    label: "Physical batch size",
    why: "The physical (GPU-kernel) batch size within each logical batch (--batch-size). Must be ≤ --batch-size; tuned alongside it for this hardware's memory/throughput tradeoff.",
    arg: "number",
  },
  "--reasoning-budget": {
    label: "Reasoning token budget",
    why: "Caps how many tokens a reasoning model may spend inside its <think> block before it's forced to produce a final answer. Prevents a model from burning its whole context on internal reasoning and never answering.",
    arg: "number",
  },
  "--chat-template-kwargs": {
    label: "Chat template kwargs",
    why: "Extra JSON kwargs passed into the Jinja chat template — on this box, almost always {\"preserve_thinking\":true|false} (gemma4-mtp-nothink instead uses {\"enable_thinking\":false} — the kwarg name itself is template-dependent). The two lineages disagree: Qwen 3.6 (qwen36/qwen36-mtp) must preserve prior turns' reasoning, but Ornith (Qwen 3.5 lineage) must NOT — froggeric v21.3's default true made Ornith burn its whole budget inside <think> with empty content (see docs/pitfalls.md). Get this backwards and the symptom looks like a reasoning-budget or template bug, not a one-line kwarg.",
    arg: "text",
    placeholder: '{"preserve_thinking":true}',
  },
  "--chat-template-file": {
    label: "Chat template override",
    why: "Replaces the GGUF's embedded chat template with an external file. Ornith needs this because its original embedded template unconditionally re-injects every prior turn's <think> block (a real broken-template bug, not the reprocessing issue tracked in docs/investigations.md item 9) — see docs/pitfalls.md.",
    arg: "text",
    placeholder: "/opt/forge/models/chat-templates/…",
  },
  "--repeat-penalty": {
    label: "Repeat penalty",
    why: "Penalizes tokens the model has already generated recently. llama-server's default (1.0) is effectively off, which lets reasoning-heavy MoE models degenerate into text-repetition loops — a sampling issue, not a template one. Ornith sets 1.08 to fix a real incident (docs/pitfalls.md).",
    arg: "number",
  },
  "--dry-multiplier": {
    label: "DRY sampler strength",
    why: "Strength of the DRY (Don't Repeat Yourself) repetition-breaking sampler, off by default (0.0). Paired with --repeat-penalty on Ornith to fix a real live text-repetition-loop incident (docs/pitfalls.md) — removing it on a model that needs it brings the loop back.",
    arg: "number",
  },
  "--temp": {
    label: "Sampling temperature",
    why: "Standard sampling temperature. Called out here only because it's part of the same curated sampler fix as --repeat-penalty/--dry-multiplier on Ornith (0.7) — changing it in isolation can reintroduce the repetition-loop symptom that fix addressed.",
    arg: "number",
  },
};

// Alias map: llama.cpp's short flags → the long-form key LLAMA_FLAGS is
// keyed on. Real live configs on this box use the short forms (laguna's
// -np/-b/-ub/-md, nemotron-puzzle's -ngl) — without this, parseLoadOptions
// would render them with no explanation and hazardsFor would silently miss
// a short-flag --parallel equivalent (-np).
const SHORT_FLAG_ALIASES: Record<string, string> = {
  "-np": "--parallel",
  "-ngl": "--n-gpu-layers",
  "-b": "--batch-size",
  "-ub": "--ubatch-size",
  "-md": "--model-draft",
  "-t": "--threads",
};

export function canonicalFlag(flag: string): string {
  return SHORT_FLAG_ALIASES[flag] ?? flag;
}

// vLLM launch-flag reference for the two configs on this box that aren't
// llama.cpp at all (carbon-8b, hy-mt2 — see ConfigCard.backend). Kept
// separate from LLAMA_FLAGS rather than merged: offering llama.cpp's flag
// set to a vLLM config would be actively wrong, not just incomplete.
export const VLLM_FLAGS: Record<string, FlagRef> = {
  "--trust-remote-code": {
    label: "Trust remote code",
    why: "Allows vLLM to execute custom modeling code shipped in the model repo rather than only vLLM's built-in architectures. Required for models vLLM doesn't natively recognize.",
    arg: "none",
  },
  "--dtype": {
    label: "Weight dtype",
    why: "The compute/storage dtype vLLM loads weights as (e.g. bfloat16). Distinct from a GGUF quantization scheme — vLLM configs on this box run unquantized bf16 weights.",
    arg: "text",
    presets: ["bfloat16", "float16", "auto"],
  },
  "--tensor-parallel-size": {
    label: "Tensor-parallel size",
    why: "Number of GPUs to shard each layer's weights across. 1 on this box (single-GPU Strix Halo) — only meaningful if this host ever gains a second GPU.",
    arg: "number",
  },
  "--gpu-memory-utilization": {
    label: "GPU memory utilization",
    why: "Fraction of GPU memory vLLM is allowed to reserve for weights + KV cache (0.98 = nearly all of it). Lowering this leaves compressor for other processes but shrinks the usable KV cache.",
    arg: "number",
  },
};

// One parsed row of a config's load-options list. `flag` is the bare
// `--name` token; `value` is its argument, or null for a boolean flag.
export interface LoadOption {
  flag: string;
  value: string | null;
  ref: FlagRef | null;
  // True when this row came from a single malformed argv token (see
  // parseLoadOptions's doc comment) rather than two well-formed ones.
  malformed?: boolean;
}

// parseLoadOptions turns a config's flat extra_args argv token array into
// display rows. extra_args is meant to be one argv element per array entry
// (go/internal/engine/sysconfig.go's writeServiceFiles writes it one per
// line, and /usr/local/lib/forge/start-a1.sh reads it back with
// `mapfile -t` — no shell word-splitting). CatalogPanel's ConfigForm used
// to teach the wrong format via its placeholder ("--ctx-checkpoints 16" on
// one line, which the line-based extraArgsFromText turns into ONE array
// element instead of two) — a config edited that way silently fails to
// start, because llama-server sees a single unknown argument instead of a
// flag and its value. This parser tolerates that malformed shape so a
// broken config is visible here rather than silently wrong, but does not
// try to guess which trailing tokens belong together for anything more
// exotic than that one well-known failure mode.
//
// Accepts single-dash short flags too (-np, -b, -ub, -md, -ngl, -t) — real
// live configs on this box use them (laguna's -np/-b/-ub/-md,
// nemotron-puzzle's -ngl); an earlier `--`-only check silently dropped all
// of them from the load-options list. `flagTable` defaults to LLAMA_FLAGS;
// pass VLLM_FLAGS for a vLLM-backend config (see ConfigCard.backend) so a
// vLLM config never gets llama.cpp's flag explanations.
export function parseLoadOptions(extraArgs: string[], flagTable: Record<string, FlagRef> = LLAMA_FLAGS): LoadOption[] {
  const out: LoadOption[] = [];
  const lookup = (flag: string) => flagTable[canonicalFlag(flag)] ?? null;
  for (let i = 0; i < extraArgs.length; i++) {
    const tok = extraArgs[i];
    if (!tok.startsWith("-")) continue; // an orphaned value with no owning flag — shouldn't happen in a well-formed list

    if (tok.includes("=")) {
      const eq = tok.indexOf("=");
      out.push({ flag: tok.slice(0, eq), value: tok.slice(eq + 1), ref: lookup(tok.slice(0, eq)) });
      continue;
    }

    if (tok.includes(" ")) {
      const sp = tok.indexOf(" ");
      const flag = tok.slice(0, sp);
      const value = tok.slice(sp + 1).trim();
      out.push({ flag, value: value || null, ref: lookup(flag), malformed: true });
      continue;
    }

    const next = extraArgs[i + 1];
    if (next != null && !next.startsWith("-")) {
      out.push({ flag: tok, value: next, ref: lookup(tok) });
      i++;
    } else {
      out.push({ flag: tok, value: null, ref: lookup(tok) });
    }
  }
  return out;
}

// serializeLoadOptions is parseLoadOptions's inverse — turns edited rows
// back into a flat extra_args token array (Sprint C's LoadOptionsEditor).
// An untouched row reproduces byte-identical tokens: `flag` is emitted
// exactly as stored (short or long spelling — never canonicalized) and
// `value`, when present, follows as its own token, matching the launcher's
// one-argv-token-per-line contract. A row with an empty flag is dropped
// (a blank "add custom flag" row the operator never filled in) rather than
// emitted as a malformed empty token. Malformed rows are NOT reproduced
// byte-identical on purpose — parseLoadOptions already split them into a
// clean flag/value pair for display, and re-serializing emits that clean
// pair, fixing the BUG-2 failure mode (docs/v5-prerelease-readiness.md)
// rather than preserving it.
export function serializeLoadOptions(rows: Pick<LoadOption, "flag" | "value">[]): string[] {
  const out: string[] = [];
  for (const r of rows) {
    if (!r.flag) continue;
    out.push(r.flag);
    if (r.value != null && r.value.length > 0) out.push(r.value);
  }
  return out;
}

// A hazard is advisory, never blocking — some hazardous flags are
// deliberate, documented choices (qwen25coder's --parallel 4). Copy should
// always read as "verify this is intentional," never "this is wrong."
export interface Hazard {
  flag: string;
  message: string;
}

// hazardsFor flags the two known live hazards on this hardware (both real
// incidents — see CLAUDE.md's "Critical Hardware / Runtime Facts"). Takes
// just the two ConfigCard fields it needs so callers (card face + expanded
// view) don't have to construct a full card.
export function hazardsFor(card: Pick<ConfigCard, "extra_args" | "n_ctx">): Hazard[] {
  const opts = parseLoadOptions(card.extra_args);
  const hazards: Hazard[] = [];

  // canonicalFlag resolves short forms (laguna's -np, etc.) so this check
  // isn't blind to a short-flag --parallel equivalent; hazard.flag is the
  // row's own raw token (not the canonical name) so ConfigDetailView's
  // hazardByFlag lookup — keyed by the raw flag shown on screen — still
  // finds it regardless of which spelling the config actually uses.
  const parallel = opts.find((o) => canonicalFlag(o.flag) === "--parallel");
  const n = parallel?.value != null ? parseInt(parallel.value, 10) : NaN;
  if (parallel && Number.isFinite(n) && n > 1) {
    const perSlot = Math.floor(card.n_ctx / n);
    hazards.push({
      flag: parallel.flag,
      message: `Context is split, not multiplied: n_ctx ${card.n_ctx.toLocaleString()} with ${parallel.flag} ${n} gives each request only ${perSlot.toLocaleString()} usable tokens. Verify this is intentional (a deliberate concurrent-short-request tradeoff) rather than a leftover default.`,
    });
  }

  const ctxCheckpoints = opts.find((o) => canonicalFlag(o.flag) === "--ctx-checkpoints");
  if (ctxCheckpoints && ctxCheckpoints.value != null && ctxCheckpoints.value !== "0") {
    hazards.push({
      flag: ctxCheckpoints.flag,
      message: "Open upstream llama.cpp hang bugs exist in this exact code path (docs/investigations.md item 9). Verify this is intentional before relying on it for a long-running conversation.",
    });
  }

  return hazards;
}
