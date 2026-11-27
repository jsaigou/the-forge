// lib/speechModel.ts — the pure logic behind Settings → Voice & Speech (docs/speech-routing-plan.md, P4): which engines make which kind of voice,
// how many voices use each engine, what the default engine for a kind is, and whether a voice can be played. Kept free of React so it is tested on
// its own (`npm test`). Erasable TypeScript only: node runs it directly.
import type { ExternalTool, SpeechDefaultsDraft, SpeechEngineInfo, SpeechKind, SpecialistBehavior, VoiceListEntry } from "./types";

export const SPEECH_KINDS: SpeechKind[] = ["kokoro", "customvoice", "voicedesign", "base"];

/** The order engines are listed in on the page (and voices grouped): the fast one first, then the kinds people make voices with, then custom voices. */
export const ENGINE_ROW_ORDER: SpeechKind[] = ["kokoro", "base", "voicedesign", "customvoice"];

/** A defaults response (only the kinds that have one) as the draft the card edits: all four kinds present, "" = none. */
export function normalizeDefaults(d: Partial<Record<SpeechKind, string>> | undefined): SpeechDefaultsDraft {
  return { kokoro: d?.kokoro ?? "", customvoice: d?.customvoice ?? "", voicedesign: d?.voicedesign ?? "", base: d?.base ?? "" };
}

/** The engines that can be chosen as the default for a kind: only engines of that kind, in name order. */
export function enginesOfKind(info: SpeechEngineInfo[], kind: SpeechKind): SpeechEngineInfo[] {
  return info.filter((e) => e.mode === kind).sort((a, b) => a.name.localeCompare(b.name));
}

/** An engine's Behavior: Disabled is `enabled=false`, otherwise its policy. */
export function engineBehavior(e: SpeechEngineInfo): SpecialistBehavior {
  return e.enabled ? e.policy : "disabled";
}

/**
 * The engine a voice of `kind` that names no engine uses: the default for the kind if it is a real engine of that kind, otherwise the first enabled
 * engine of the kind (the daemon's own order), otherwise none. Mirrors the daemon (resolution steps 3 and 4) so the page agrees with what is played.
 */
export function defaultEngineFor(kind: string, defaults: SpeechDefaultsDraft, info: SpeechEngineInfo[]): SpeechEngineInfo | undefined {
  const ofKind = enginesOfKind(info, kind as SpeechKind);
  const chosen = defaults[kind as SpeechKind];
  const hit = chosen ? ofKind.find((e) => e.name === chosen) : undefined;
  return hit ?? ofKind.find((e) => e.enabled);
}

export type VoiceStatus = "ready" | "missing" | "off";

export interface VoiceUse {
  /** The engine that will make this voice ("" when there is none). */
  engine: string;
  /** True when the voice names no engine and the kind's default applies. */
  viaDefault: boolean;
  status: VoiceStatus;
}

/**
 * Which engine a voice uses and whether it can be played. A voice that names an engine is never moved to another: if that engine does not exist it is
 * "missing", if it is turned off it is "off". A voice that names none uses the default for its kind; with no usable engine of the kind it is "missing".
 */
export function voiceUse(v: Pick<VoiceListEntry, "kind" | "engine">, defaults: SpeechDefaultsDraft, info: SpeechEngineInfo[]): VoiceUse {
  if (v.engine) {
    const e = info.find((x) => x.name === v.engine);
    if (!e) return { engine: v.engine, viaDefault: false, status: "missing" };
    return { engine: e.name, viaDefault: false, status: e.enabled ? "ready" : "off" };
  }
  const d = defaultEngineFor(v.kind, defaults, info);
  if (!d) {
    // No usable engine of the kind. If the kind has engines at all they are all off; otherwise there is none.
    const any = enginesOfKind(info, v.kind as SpeechKind)[0];
    return { engine: any?.name ?? "", viaDefault: true, status: any ? "off" : "missing" };
  }
  return { engine: d.name, viaDefault: true, status: "ready" };
}

/** Voices per engine name. A voice that names no engine counts toward the engine its kind's default resolves to. */
export function countVoicesByEngine(voices: VoiceListEntry[], defaults: SpeechDefaultsDraft, info: SpeechEngineInfo[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const v of voices) {
    const use = voiceUse(v, defaults, info);
    if (!use.engine) continue;
    counts.set(use.engine, (counts.get(use.engine) ?? 0) + 1);
  }
  return counts;
}

/** Voices that cannot be played right now (their engine is missing or off): missing first, then by id. */
export function unplayableVoices(voices: VoiceListEntry[], defaults: SpeechDefaultsDraft, info: SpeechEngineInfo[]): VoiceListEntry[] {
  const rank = (s: VoiceStatus) => (s === "missing" ? 0 : s === "off" ? 1 : 2);
  return voices
    .map((v) => ({ v, s: voiceUse(v, defaults, info).status }))
    .filter((x) => x.s !== "ready")
    .sort((a, b) => rank(a.s) - rank(b.s) || a.v.id.localeCompare(b.v.id))
    .map((x) => x.v);
}

/** The Voice Studio entry among the operator's external tools, found by its name or address; undefined when there is none. */
export function voiceStudioLink(tools: ExternalTool[] | undefined): ExternalTool | undefined {
  const named = /voice[\s_-]?studio/i;
  return tools?.find((t) => named.test(t.name) || named.test(t.url));
}
