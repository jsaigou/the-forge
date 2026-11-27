// The web app's Config types must list exactly the fields the Go API serves and accepts. The golden list is written by the Go side
// (go/internal/httpapi/catalog_configs_fields_test.go); CATALOG_CONFIG_KEYS in configPayload.ts is exhaustive over CatalogConfig at compile
// time, so comparing it here closes the loop: Go struct -> golden file -> TypeScript type.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { CATALOG_CONFIG_KEYS, CONFIG_READ_ONLY_KEYS, toConfigWritePayload } from "../src/lib/configPayload.ts";

const golden = JSON.parse(readFileSync(new URL("../src/lib/configFields.golden.json", import.meta.url), "utf8"));
const sorted = (a) => [...a].sort();

test("CatalogConfig lists the fields the Go API returns", () => {
  assert.deepEqual(sorted(Object.keys(CATALOG_CONFIG_KEYS)), golden.view);
});

test("a write payload carries exactly the fields the Go API accepts", () => {
  const full = Object.fromEntries(Object.keys(CATALOG_CONFIG_KEYS).map((k) => [k, k === "name" ? "x" : 0]));
  assert.deepEqual(sorted(Object.keys(toConfigWritePayload(full))), golden.write);
});

test("toConfigWritePayload strips the read-only fields and applies the changes", () => {
  const full = Object.fromEntries(Object.keys(CATALOG_CONFIG_KEYS).map((k) => [k, 1]));
  const out = toConfigWritePayload(full, { logo: "mine" });
  for (const k of CONFIG_READ_ONLY_KEYS) assert.ok(!(k in out), `${k} must not be written`);
  assert.equal(out.logo, "mine");
  assert.equal(out.n_ctx, 1, "fields the caller did not touch carry through");
});
