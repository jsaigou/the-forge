// SPDX-License-Identifier: Apache-2.0
//
// safeHref gates every href whose value comes from data (catalog rows, service links, search results, model-written markdown).
// React does not block javascript: URLs, so a stored `javascript:alert(1)` source_url or license_url would run on click.
// Only absolute http(s) URLs pass; anything else (javascript:, data:, vbscript:, file:, relative, malformed, empty) yields undefined,
// and the caller renders plain text instead of a link.

export function safeHref(value: string | null | undefined): string | undefined {
  if (typeof value !== "string") return undefined;
  const v = value.trim();
  if (v === "") return undefined;
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    return undefined;
  }
  return u.protocol === "http:" || u.protocol === "https:" ? v : undefined;
}

// safeLink is safeHref for in-app prose (model-written markdown) where guidance legitimately points inside the app: it also allows a
// same-site path ("/settings#catalog"), a "#fragment" and mailto:. Protocol-relative ("//host"), backslash-padded and control-character forms
// are refused, since browsers read those as off-site.
export function safeLink(value: string | null | undefined): string | undefined {
  if (typeof value !== "string") return undefined;
  const v = value.trim();
  if (v === "") return undefined;
  if (v.startsWith("#")) return /[\u0000-\u001f\u007f\\]/.test(v) ? undefined : v;
  if (v.startsWith("/")) {
    if (v.startsWith("//") || /[\u0000-\u001f\u007f\\]/.test(v)) return undefined;
    return v;
  }
  const abs = safeHref(v);
  if (abs) return abs;
  try {
    return new URL(v).protocol === "mailto:" ? v : undefined;
  } catch {
    return undefined;
  }
}
