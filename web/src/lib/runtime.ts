import { LLAMA_FLAGS, RADIANCE_FLAGS, VLLM_FLAGS, type FlagRef } from "./llamaFlags.ts";
import { own } from "./own.ts";

// The "runtime" of a config is the inference software that serves it: llama.cpp, vLLM or Radiance (UI word per
// locales/GLOSSARY.md; "engine" is taken by dedicated-slot engines). Its identity is the server-derived kind
// (ConfigCard.engine_kind, engines.kind resolved in go/internal/enginekind), never the Build's `backend`, which is only
// the GPU path (rocm | vulkan).

// Product names; not translated.
const RUNTIME_LABELS: Record<string, string> = {
  llamacpp: "llama.cpp",
  vllm: "vLLM",
  radiance: "Radiance",
};

const FLAG_TABLES: Record<string, { ns: string; table: Record<string, FlagRef> }> = {
  llamacpp: { ns: "llama_flags", table: LLAMA_FLAGS },
  vllm: { ns: "vllm_flags", table: VLLM_FLAGS },
  radiance: { ns: "radiance_flags", table: RADIANCE_FLAGS },
};

// Mirrors enginekind.Resolve for the client-side case where only an engine row's kind and a build's backend are known (the
// config editor, before a save): the legacy backend "vllm" reads as vLLM unless the engine says something more specific. An
// unknown kind passes through unchanged; an empty kind falls back to llama.cpp only when nothing at all is known.
export function resolveRuntimeKind(kind: string | null | undefined, backend?: string | null): string {
  const k = kind ?? "";
  if (backend === "vllm" && (k === "" || k === "llamacpp")) return "vllm";
  return k === "" ? "llamacpp" : k;
}

// Display name for a kind. An unknown kind shows as the raw kind (never crashes, never hides it); a missing kind shows a dash.
export function runtimeLabel(kind: string | null | undefined): string {
  if (!kind) return "—";
  return own(RUNTIME_LABELS, kind) ?? kind;
}

// The curated flag table for a kind. An unknown kind gets an empty table: its flags render as plain rows with no
// explanation, rather than another runtime's explanations (which would be actively wrong).
export function flagTableForKind(kind: string | null | undefined): Record<string, FlagRef> {
  return own(FLAG_TABLES, kind ?? "")?.table ?? {};
}

// The common.json namespace holding the translated label/why for a kind's flags; "" when the kind has none.
export function flagNamespaceForKind(kind: string | null | undefined): string {
  return own(FLAG_TABLES, kind ?? "")?.ns ?? "";
}

// Flags a kind refuses to launch without. Radiance has no defaults: --max-num-seqs must be set per model.
export function requiredFlagsForKind(kind: string | null | undefined): string[] {
  return kind === "radiance" ? ["--max-num-seqs"] : [];
}
