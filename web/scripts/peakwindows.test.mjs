// Tests for src/lib/peakWindows.ts (the provider peak-windows builder). Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { newWindowRow, parseWindowRows, serializeWindowRows, toggleDay } from "../src/lib/peakWindows.ts";

test("parses the documented example and round-trips", () => {
  const json = `[{"days":[1,2,3,4,5],"start":"01:00","end":"04:00"}]`;
  const rows = parseWindowRows(json);
  assert.deepEqual(rows, [{ days: [1, 2, 3, 4, 5], start: "01:00", end: "04:00" }]);
  assert.equal(serializeWindowRows(rows), json);
});

test("empty text means no rows, and no rows serialize to empty text", () => {
  assert.deepEqual(parseWindowRows(""), []);
  assert.deepEqual(parseWindowRows("  "), []);
  assert.equal(serializeWindowRows([]), "");
});

test("anything the builder cannot represent returns null (raw editor stays open)", () => {
  for (const bad of ["{", "{}", `[1]`, `[{"days":[7],"start":"01:00","end":"02:00"}]`, `[{"days":[1],"start":"1:00","end":"02:00"}]`, `[{"days":"mon","start":"01:00","end":"02:00"}]`, `[{"days":[1],"start":"01:00"}]`]) {
    assert.equal(parseWindowRows(bad), null, bad);
  }
});

test("days are normalised to sorted order", () => {
  assert.deepEqual(parseWindowRows(`[{"days":[5,1],"start":"01:00","end":"02:00"}]`)[0].days, [1, 5]);
});

test("toggleDay adds in order and removes", () => {
  assert.deepEqual(toggleDay([1, 3], 2), [1, 2, 3]);
  assert.deepEqual(toggleDay([1, 2, 3], 2), [1, 3]);
});

test("a new row is weekday working hours", () => {
  assert.deepEqual(newWindowRow(), { days: [1, 2, 3, 4, 5], start: "09:00", end: "17:00" });
});
