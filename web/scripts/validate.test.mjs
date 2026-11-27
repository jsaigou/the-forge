// Tests for src/settings/validate.ts (registry-driven field validation). Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { validateField } from "../src/settings/validate.ts";

test("number range is checked in displayed units (scale applied)", () => {
  const rec = { input: "number", min: 0, max: 100, scale: 1000 };
  assert.equal(validateField(rec, 100), null); // stored 100 = 0.1 shown
  assert.equal(validateField(rec, 100_000), null);
  assert.deepEqual(validateField(rec, 100_001)?.key, "validation.max");
  assert.deepEqual(validateField({ input: "number", min: 1 }, 0)?.key, "validation.min");
});

test("required applies to empty text and empty number strings", () => {
  assert.equal(validateField({ input: "text", required: true }, "  ")?.key, "validation.required");
  assert.equal(validateField({ input: "text" }, ""), null);
  assert.equal(validateField({ input: "number", required: true }, "")?.key, "validation.required");
});

test("pattern is only checked when the value is non-empty", () => {
  const rec = { input: "text", pattern: "^https?://", patternHint: "an http(s) URL" };
  assert.equal(validateField(rec, ""), null);
  assert.equal(validateField(rec, "ftp://x")?.key, "validation.pattern");
  assert.equal(validateField(rec, "http://x"), null);
});

test("durations validate and range-check in seconds", () => {
  const rec = { input: "duration", min: 60, max: 86400 };
  assert.equal(validateField(rec, "5m"), null);
  assert.equal(validateField(rec, "banana")?.key, "validation.duration");
  assert.equal(validateField(rec, "30s")?.key, "validation.min");
  assert.equal(validateField(rec, "48h")?.key, "validation.max");
  assert.equal(validateField(rec, ""), null);
});

test("toggles and selects never error", () => {
  assert.equal(validateField({ input: "toggle", required: true }, false), null);
});
