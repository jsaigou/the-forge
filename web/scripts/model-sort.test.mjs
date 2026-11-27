// Tests for src/lib/modelSort.ts (the Console's config gallery). Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { defaultConfigSort, sortConfigCards } from "../src/lib/modelSort.ts";

const card = (id, name) => ({ id, name, model_id: id, is_default: false, created_at: id, derived: {} });
const cards = [card(2, "beta"), card(1, "alpha")];

test("the gallery opens on everything until something is starred", () => {
  assert.equal(defaultConfigSort(cards, new Set()), "alpha");
  assert.equal(defaultConfigSort(cards, new Set([99])), "alpha"); // a star on a config that is gone
  assert.equal(defaultConfigSort([], new Set([1])), "alpha");
  assert.equal(defaultConfigSort(cards, new Set([2])), "favorites");
});

test("favorites shows only starred configs; the other views show all, starred first", () => {
  assert.deepEqual(sortConfigCards(cards, "favorites", new Set([2])).map((c) => c.name), ["beta"]);
  assert.deepEqual(sortConfigCards(cards, "alpha", new Set()).map((c) => c.name), ["alpha", "beta"]);
  assert.deepEqual(sortConfigCards(cards, "alpha", new Set([2])).map((c) => c.name), ["beta", "alpha"]);
});
