// Tests for src/lib/speechModel.ts (the logic behind Settings → Voice & Speech). Run with `npm test` (node's built-in runner; the module is
// erasable TypeScript, so node loads it directly).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  countVoicesByEngine,
  defaultEngineFor,
  engineBehavior,
  enginesOfKind,
  normalizeDefaults,
  unplayableVoices,
  voiceStudioLink,
  voiceUse,
} from "../src/lib/speechModel.ts";

const eng = (name, mode, over = {}) => ({ name, label: name, mode, policy: "always_on", enabled: true, stopped: false, state: "running", ...over });
const INFO = [
  eng("qwen3-tts-voicedesign", "voicedesign", { policy: "on_demand" }),
  eng("qwen3-tts-base", "base"),
  eng("qwen3-tts-customvoice", "customvoice", { enabled: false, state: "idle" }),
  eng("kokoro", "kokoro"),
];
const voice = (id, kind, engine = "") => ({ id, name: id, type: kind, kind, engine, language: "English", builtin: false, has_sample: true });
const DEFAULTS = normalizeDefaults({ base: "qwen3-tts-base", voicedesign: "qwen3-tts-voicedesign", kokoro: "kokoro" });

test("normalizeDefaults always has all four kinds, empty when unset", () => {
  assert.deepEqual(normalizeDefaults(undefined), { kokoro: "", customvoice: "", voicedesign: "", base: "" });
  assert.deepEqual(normalizeDefaults({ base: "b" }), { kokoro: "", customvoice: "", voicedesign: "", base: "b" });
});

test("the default pickers list only engines of their own kind", () => {
  assert.deepEqual(enginesOfKind(INFO, "base").map((e) => e.name), ["qwen3-tts-base"]);
  assert.deepEqual(enginesOfKind(INFO, "customvoice").map((e) => e.name), ["qwen3-tts-customvoice"]);
  const more = [...INFO, eng("base-b", "base"), eng("a-base", "base")];
  assert.deepEqual(enginesOfKind(more, "base").map((e) => e.name), ["a-base", "base-b", "qwen3-tts-base"]);
  assert.deepEqual(enginesOfKind(INFO, "voicedesign").map((e) => e.mode), ["voicedesign"]);
});

test("Behavior: Disabled is enabled=false, otherwise the policy", () => {
  assert.equal(engineBehavior(eng("a", "base")), "always_on");
  assert.equal(engineBehavior(eng("a", "base", { policy: "on_demand" })), "on_demand");
  assert.equal(engineBehavior(eng("a", "base", { enabled: false })), "disabled");
});

test("a voice that names no engine uses the kind's default, else the first enabled engine of the kind", () => {
  assert.equal(defaultEngineFor("base", DEFAULTS, INFO)?.name, "qwen3-tts-base");
  const two = [...INFO, eng("a-base", "base")];
  assert.equal(defaultEngineFor("base", normalizeDefaults({}), two)?.name, "a-base");
  // a default naming an engine that no longer exists falls through to the first enabled one
  assert.equal(defaultEngineFor("base", normalizeDefaults({ base: "gone" }), INFO)?.name, "qwen3-tts-base");
  // a kind whose only engine is off has no default engine
  assert.equal(defaultEngineFor("customvoice", DEFAULTS, INFO), undefined);
});

test("voice status: ready, engine missing, engine off, and never another engine", () => {
  assert.deepEqual(voiceUse(voice("a", "base", "qwen3-tts-base"), DEFAULTS, INFO), { engine: "qwen3-tts-base", viaDefault: false, status: "ready" });
  assert.deepEqual(voiceUse(voice("a", "base", "base-b"), DEFAULTS, INFO), { engine: "base-b", viaDefault: false, status: "missing" });
  assert.deepEqual(voiceUse(voice("a", "customvoice", "qwen3-tts-customvoice"), DEFAULTS, INFO), { engine: "qwen3-tts-customvoice", viaDefault: false, status: "off" });
  // named engine that is missing is NOT rescued by the default for its kind
  assert.equal(voiceUse(voice("a", "base", "base-b"), DEFAULTS, INFO).engine, "base-b");
  // no engine named: default applies
  assert.deepEqual(voiceUse(voice("s", "voicedesign"), DEFAULTS, INFO), { engine: "qwen3-tts-voicedesign", viaDefault: true, status: "ready" });
  // kind with only an off engine: off, naming it; kind with no engine at all: missing
  assert.deepEqual(voiceUse(voice("c", "customvoice"), DEFAULTS, INFO), { engine: "qwen3-tts-customvoice", viaDefault: true, status: "off" });
  assert.deepEqual(voiceUse(voice("x", "base"), DEFAULTS, INFO.filter((e) => e.mode !== "base")), { engine: "", viaDefault: true, status: "missing" });
});

test("voices are counted per engine, unbound voices toward their kind's default", () => {
  const voices = [
    voice("sana", "base", "qwen3-tts-base"), voice("ann", "base"), voice("zed", "base", "base-b"),
    voice("bert", "voicedesign", "qwen3-tts-voicedesign"), voice("susan", "voicedesign"),
    voice("af_heart", "kokoro"), voice("am_adam", "kokoro"),
  ];
  const c = countVoicesByEngine(voices, DEFAULTS, INFO);
  assert.equal(c.get("qwen3-tts-base"), 2); // sana + ann (default); zed names a missing engine
  assert.equal(c.get("qwen3-tts-voicedesign"), 2);
  assert.equal(c.get("kokoro"), 2);
  assert.equal(c.get("base-b"), 1); // counted against the engine it names even if that engine does not exist
  assert.equal(c.get("qwen3-tts-customvoice"), undefined);
});

test("unplayable voices come first: missing before off, then by id", () => {
  const voices = [
    voice("ok", "base", "qwen3-tts-base"), voice("z-off", "customvoice", "qwen3-tts-customvoice"),
    voice("b-gone", "base", "nope"), voice("a-gone", "base", "nope-2"),
  ];
  assert.deepEqual(unplayableVoices(voices, DEFAULTS, INFO).map((v) => v.id), ["a-gone", "b-gone", "z-off"]);
});

test("voiceStudioLink finds Voice Studio among the external tools by name or address", () => {
  const talk = { name: "Talk Studio", url: "https://talk.example/" };
  assert.equal(voiceStudioLink(undefined), undefined);
  assert.equal(voiceStudioLink([talk]), undefined);
  assert.equal(voiceStudioLink([talk, { name: "Voice Studio", url: "https://v.example/" }]).url, "https://v.example/");
  assert.equal(voiceStudioLink([{ name: "Voices", url: "https://voice-studio.example/" }]).name, "Voices");
});

