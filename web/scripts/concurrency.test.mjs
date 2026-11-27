// Tests for src/lib/concurrency.ts (the mapping/formatting behind the Dashboard Concurrency card). Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  CONCURRENCY_WINDOWS,
  concurrencyRange,
  formatBusy,
  formatFraction,
  liveFor,
  modelsSeen,
  outcomeCounts,
} from "../src/lib/concurrency.ts";

test("windows map to since/until in whole unix seconds", () => {
  assert.deepEqual(concurrencyRange("1h", 10_000.9), { since: 10_000 - 3600, until: 10_000 });
  const d = concurrencyRange("24h", 1_000_000);
  assert.equal(d.until - d.since, 86_400);
  const w = concurrencyRange("7d", 1_000_000);
  assert.equal(w.until - w.since, 7 * 86_400);
});

test("no window exceeds the backend's 31 day cap", () => {
  for (const w of CONCURRENCY_WINDOWS) {
    const r = concurrencyRange(w, 5_000_000);
    assert.ok(r.until - r.since <= 31 * 86_400, w);
  }
});

test("models seen merges local, external and live, sorted and de-duplicated", () => {
  const live = { peak_window_s: 900, rows: [{ model: "b", in_flight: 1, peak: 2 }, { model: "z", in_flight: 0, peak: 1 }] };
  assert.deepEqual(modelsSeen([{ model: "b" }, { model: "" }], [{ model: "a" }, { model: "b" }], live), ["a", "b", "z"]);
  assert.deepEqual(modelsSeen(undefined, undefined, undefined), []);
});

test("live gauge uses the model-total row, not a per-consumer row", () => {
  const live = {
    peak_window_s: 900,
    rows: [
      { model: "m", consumer: "opencode", in_flight: 9, peak: 9 },
      { model: "m", in_flight: 2, peak: 5 },
    ],
  };
  assert.deepEqual(liveFor(live, "m"), { now: 2, peak: 5 });
  assert.equal(liveFor(live, "other"), null);
  assert.equal(liveFor(undefined, "m"), null);
});

test("missing outcome keys are zero", () => {
  assert.deepEqual(outcomeCounts({ ok: 3 }), { ok: 3, error: 0, aborted: 0 });
  assert.deepEqual(outcomeCounts(undefined), { ok: 0, error: 0, aborted: 0 });
});

test("overlap fraction keeps a small real value visible and clamps", () => {
  assert.equal(formatFraction(0, "en-US"), "0%");
  assert.equal(formatFraction(0.004, "en-US"), "0.4%");
  assert.equal(formatFraction(0.123, "en-US"), "12%");
  assert.equal(formatFraction(1.7, "en-US"), "100%");
  assert.equal(formatFraction(NaN, "en-US"), "0%");
});

test("busy time picks seconds, minutes or hours", () => {
  const u = { s: "s", min: "min", h: "h" };
  assert.deepEqual(formatBusy(4.25, u, "en-US"), { value: "4.3", unit: "s" });
  assert.deepEqual(formatBusy(42, u, "en-US"), { value: "42", unit: "s" });
  assert.deepEqual(formatBusy(318, u, "en-US"), { value: "5.3", unit: "min" });
  assert.deepEqual(formatBusy(8640, u, "en-US"), { value: "2.4", unit: "h" });
  assert.deepEqual(formatBusy(-1, u, "en-US"), { value: "0", unit: "s" });
});
