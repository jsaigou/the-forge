// Prototype-chain lookups on catalog/operator-supplied strings: "constructor" and "__proto__" must behave like any unknown key. Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { own } from "../src/lib/own.ts";
import { runtimeLabel, flagTableForKind, flagNamespaceForKind } from "../src/lib/runtime.ts";
import { capLabel } from "../src/lib/chatTemplateCaps.ts";
import { normalizeScore } from "../src/lib/benchmarks.ts";
import { rangeLabel } from "../src/lib/rangeLabels.ts";
import { providerIconSlug } from "../src/lib/providerPresets.ts";

const HOSTILE = ["constructor", "__proto__", "toString", "hasOwnProperty", "valueOf"];

test("own() sees only own properties", () => {
  const t = { a: 1 };
  assert.equal(own(t, "a"), 1);
  for (const k of HOSTILE) assert.equal(own(t, k), undefined, k);
  assert.equal(own(t, undefined), undefined);
  assert.equal(own(t, null), undefined);
});

test("runtime tables treat inherited names as unknown kinds", () => {
  for (const k of HOSTILE) {
    assert.equal(runtimeLabel(k), k);
    assert.deepEqual(flagTableForKind(k), {});
    assert.equal(flagNamespaceForKind(k), "");
  }
});

test("capLabel falls through to the humanised key", () => {
  assert.equal(capLabel("constructor"), "Constructor");
  assert.equal(capLabel("__proto__"), "Proto");
});

test("normalizeScore clamps instead of reading an inherited reference", () => {
  for (const k of HOSTILE) assert.equal(normalizeScore(0.5, k), 0.5, k);
});

test("rangeLabel and provider slugs return the raw input for hostile keys", () => {
  for (const k of HOSTILE) {
    assert.equal(rangeLabel(k, (x) => `T:${x}`), k);
    assert.equal(providerIconSlug(k), k.toLowerCase());
  }
});
