// Translation lookup for lib/llamaFlags.ts's FlagRef.label/.why, kept out of
// llamaFlags.ts itself so that file's LLAMA_FLAGS/VLLM_FLAGS tables stay
// pure English data (matched byte-for-byte against docs/pitfalls.md and
// docs/investigations.md citations in code review). `isVllm` picks which
// namespace a flag name was drawn from — LLAMA_FLAGS and VLLM_FLAGS never
// share a key, but a caller only has the flag name and ref, not which table
// it came from, so it has to say so explicitly.
type Translator = (key: string, opts?: Record<string, unknown>) => string;

export function translatedFlagLabel(t: Translator, isVllm: boolean, flagName: string, fallback: string): string {
  const ns = isVllm ? "vllm_flags" : "llama_flags";
  return t(`common:${ns}.${flagName}.label`, { defaultValue: fallback });
}

export function translatedFlagWhy(t: Translator, isVllm: boolean, flagName: string, fallback: string): string {
  const ns = isVllm ? "vllm_flags" : "llama_flags";
  return t(`common:${ns}.${flagName}.why`, { defaultValue: fallback });
}
