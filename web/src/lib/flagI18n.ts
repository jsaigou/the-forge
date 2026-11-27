// Translation lookup for lib/llamaFlags.ts's FlagRef.label/.why, kept out of
// llamaFlags.ts itself so that file's LLAMA_FLAGS/VLLM_FLAGS/RADIANCE_FLAGS
// tables stay pure English data (matched byte-for-byte against
// docs/pitfalls.md and docs/investigations.md citations in code review).
// `kind` is the config's runtime kind (ConfigCard.engine_kind); it picks which
// namespace a flag name was drawn from — the tables never share a key, but a
// caller only has the flag name and ref, not which table it came from. A kind
// with no table has no refs, so nothing reaches here for it; the fallback text
// is returned untranslated.
import { flagNamespaceForKind } from "./runtime.ts";

type Translator = (key: string, opts?: Record<string, unknown>) => string;

export function translatedFlagLabel(t: Translator, kind: string, flagName: string, fallback: string): string {
  const ns = flagNamespaceForKind(kind);
  return ns ? t(`common:${ns}.${flagName}.label`, { defaultValue: fallback }) : fallback;
}

export function translatedFlagWhy(t: Translator, kind: string, flagName: string, fallback: string): string {
  const ns = flagNamespaceForKind(kind);
  return ns ? t(`common:${ns}.${flagName}.why`, { defaultValue: fallback }) : fallback;
}
