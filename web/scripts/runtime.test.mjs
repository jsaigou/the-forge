// The runtime (inference software) of a config is chosen by the server-derived kind, never by the build's backend.
import test from "node:test";
import assert from "node:assert/strict";
import { flagNamespaceForKind, flagTableForKind, requiredFlagsForKind, resolveRuntimeKind, runtimeLabel } from "../src/lib/runtime.ts";
import { translatedFlagLabel, translatedFlagWhy } from "../src/lib/flagI18n.ts";
import { LLAMA_FLAGS, RADIANCE_FLAGS, VLLM_FLAGS } from "../src/lib/llamaFlags.ts";
import { readFileSync } from "node:fs";

test("kind to label, with a fallback that shows the raw kind", () => {
  assert.equal(runtimeLabel("llamacpp"), "llama.cpp");
  assert.equal(runtimeLabel("vllm"), "vLLM");
  assert.equal(runtimeLabel("radiance"), "Radiance");
  assert.equal(runtimeLabel("somefuture"), "somefuture");
  assert.equal(runtimeLabel(""), "—");
  assert.equal(runtimeLabel(undefined), "—");
});

test("kind to flag table and namespace", () => {
  assert.equal(flagTableForKind("llamacpp"), LLAMA_FLAGS);
  assert.equal(flagTableForKind("vllm"), VLLM_FLAGS);
  assert.equal(flagTableForKind("radiance"), RADIANCE_FLAGS);
  assert.equal(flagNamespaceForKind("radiance"), "radiance_flags");
  assert.equal(flagNamespaceForKind("vllm"), "vllm_flags");
  assert.equal(flagNamespaceForKind("llamacpp"), "llama_flags");
});

test("an unknown kind gets no flag table and no namespace, and does not throw", () => {
  assert.deepEqual(flagTableForKind("somefuture"), {});
  assert.equal(flagNamespaceForKind("somefuture"), "");
  const t = () => { throw new Error("must not translate"); };
  assert.equal(translatedFlagLabel(t, "somefuture", "--x", "fallback"), "fallback");
  assert.equal(translatedFlagWhy(t, "somefuture", "--x", "fallback why"), "fallback why");
});

test("resolveRuntimeKind keeps the legacy backend vllm rule and passes unknown kinds through", () => {
  assert.equal(resolveRuntimeKind("llamacpp", "rocm"), "llamacpp");
  assert.equal(resolveRuntimeKind("llamacpp", "vllm"), "vllm");
  assert.equal(resolveRuntimeKind("", "vllm"), "vllm");
  assert.equal(resolveRuntimeKind("radiance", "vllm"), "radiance");
  assert.equal(resolveRuntimeKind("radiance", "rocm"), "radiance");
  assert.equal(resolveRuntimeKind("somefuture", "rocm"), "somefuture");
  assert.equal(resolveRuntimeKind(undefined, undefined), "llamacpp");
});

test("Radiance requires --max-num-seqs; the other runtimes require nothing", () => {
  assert.deepEqual(requiredFlagsForKind("radiance"), ["--max-num-seqs"]);
  assert.deepEqual(requiredFlagsForKind("llamacpp"), []);
  assert.deepEqual(requiredFlagsForKind("vllm"), []);
});

test("Radiance flags offer no presets, and every flag has en and ja text", () => {
  const want = ["--tp", "--max-num-seqs", "--kv-cache-dtype", "--num-speculative-tokens", "--max-model-len", "--kernels"].sort();
  assert.deepEqual(Object.keys(RADIANCE_FLAGS).sort(), want);
  for (const ref of Object.values(RADIANCE_FLAGS)) assert.equal(ref.presets, undefined);
  for (const lang of ["en", "ja"]) {
    const common = JSON.parse(readFileSync(new URL(`../src/locales/${lang}/common.json`, import.meta.url), "utf8"));
    assert.deepEqual(Object.keys(common.radiance_flags).sort(), want);
    for (const f of want) assert.ok(common.radiance_flags[f].label && common.radiance_flags[f].why, `${lang} ${f}`);
  }
});

test("translated lookup uses the kind's namespace", () => {
  const seen = [];
  const t = (key, opts) => { seen.push(key); return opts.defaultValue; };
  translatedFlagLabel(t, "radiance", "--tp", "x");
  translatedFlagWhy(t, "vllm", "--dtype", "y");
  assert.deepEqual(seen, ["common:radiance_flags.--tp.label", "common:vllm_flags.--dtype.why"]);
});
