// Tests for src/lib/safeHref.ts: stored URLs reach <a href>, and React does not block javascript: URLs. Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { safeHref } from "../src/lib/safeHref.ts";

test("http and https URLs pass through unchanged", () => {
  assert.equal(safeHref("https://huggingface.co/org/repo"), "https://huggingface.co/org/repo");
  assert.equal(safeHref("http://10.0.0.5:8085/x?y=1#z"), "http://10.0.0.5:8085/x?y=1#z");
  assert.equal(safeHref("  https://example.com  "), "https://example.com");
  assert.equal(safeHref("HTTPS://EXAMPLE.COM/"), "HTTPS://EXAMPLE.COM/");
});

test("script-capable and non-web schemes are refused, whatever the casing or padding", () => {
  for (const bad of [
    "javascript:alert(1)", "JaVaScRiPt:alert(1)", " javascript:alert(1)", "\tjavascript:alert(1)", "java\nscript:alert(1)",
    "data:text/html,<script>alert(1)</script>", "vbscript:msgbox(1)", "file:///etc/passwd", "ftp://example.com/", "blob:https://example.com/x",
    "javascript://example.com/%0aalert(1)",
  ]) {
    assert.equal(safeHref(bad), undefined, JSON.stringify(bad));
  }
});

test("relative, empty and malformed values are refused", () => {
  for (const bad of ["", "   ", "/relative", "//example.com", "example.com", "not a url", "http://", null, undefined]) {
    assert.equal(safeHref(bad), undefined, JSON.stringify(bad));
  }
});

import { safeLink } from "../src/lib/safeHref.ts";

test("safeLink allows in-app paths, anchors and mailto, but nothing off-site or scripted", () => {
  for (const ok of ["https://example.com/x", "http://10.0.0.5:8085/a", "/settings#catalog", "/a/b?c=1", "#section", "mailto:ops@example.com"]) {
    assert.equal(safeLink(ok), ok, ok);
  }
  for (const bad of ["//evil.example/x", "/\\evil.example", "/\tx", "javascript:alert(1)", "JAVASCRIPT:alert(1)", "data:text/html,x", "vbscript:x", "file:///etc/passwd", "ftp://x/y", "relative/path", "", "  ", null, undefined, "#a\\b"]) {
    assert.equal(safeLink(bad), undefined, JSON.stringify(bad));
  }
});
