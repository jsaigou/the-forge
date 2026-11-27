// Tests for src/lib/duration.ts (the logic behind the settings duration input). Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { durationSeconds, formatDuration, parseDuration } from "../src/lib/duration.ts";

test("parses single-unit durations", () => {
  assert.deepEqual(parseDuration("60m"), { n: 1, unit: "h" });
  assert.deepEqual(parseDuration("45m"), { n: 45, unit: "m" });
  assert.deepEqual(parseDuration("6h"), { n: 6, unit: "h" });
  assert.deepEqual(parseDuration("90s"), { n: 90, unit: "s" });
});

test("composite durations normalise to the largest even unit", () => {
  assert.deepEqual(parseDuration("1h30m"), { n: 90, unit: "m" });
  assert.deepEqual(parseDuration("2h"), { n: 2, unit: "h" });
});

test("zero and fractions", () => {
  assert.deepEqual(parseDuration("0"), { n: 0, unit: "s" });
  assert.deepEqual(parseDuration("1.5h"), { n: 90, unit: "m" });
  assert.deepEqual(parseDuration("1.5s"), { n: 1.5, unit: "s" });
});

test("unrepresentable input returns null so the raw text stays editable", () => {
  for (const bad of ["", "abc", "10", "5ms", "1d", "1h 30m", "-5m"]) {
    assert.equal(parseDuration(bad), null, bad);
  }
  assert.equal(durationSeconds("5x"), null);
});

test("formatDuration round-trips through parseDuration", () => {
  assert.equal(formatDuration(30, "m"), "30m");
  assert.deepEqual(parseDuration(formatDuration(30, "m")), { n: 30, unit: "m" });
});
