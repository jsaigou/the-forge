import { formatCurrencyPrecise, formatDurationShort, formatTokens } from "../lib/format";
import { useCompressorSummary } from "../lib/queries";
import type { CompressorSummaryProxy } from "../lib/types";

// Compressor savings, split into two honest chips instead of one blended
// "Compressor saved" figure (the local estimate and a remote figure used to be
// summed together, which hid which figure was real). Shared by Dashboard's
// Overview/Cost tabs and Console's Resources section — all three just pick a
// window; useCompressorSummary is React-Query-cached, so same-window callers
// on the same page share one fetch.
//
// The external chip prices Compressor's OWN compression saving (tokens_saved →
// compression_saved_native/display), not the provider's prompt-cache discount
// (cache_discount_saved_native) — that discount applies with or without
// Compressor in the request path, so crediting it to Compressor was wrong (fixed
// 2026-07-31). The provider-cache figure is still real and still shown, just
// on the per-proxy ProxyCard in Dashboard's Cost tab, correctly labelled as
// the provider's own discount rather than a Compressor saving.
//
// Operator follow-up, same day: one line per chip ("950T / ¥0.02"), no
// subtext — the estimate/source disclosure moves to a hover tooltip instead.
// This also required a real backend fix: the endpoint never FX-converted its
// money fields (billing.display_currency=JPY still showed a raw USD figure),
// and formatCurrency's fixed 2 fraction digits rounded the genuinely-tiny
// real saving to a misleading "$0.00" — see formatCurrencyPrecise.
//
// Pre-release feedback round, Phase 5 (2026-08-12): the external chip showed
// "950T - ¥0.02" (tokens AND money) while the local chip showed only a
// duration — an asymmetry the operator flagged directly. Fixed by dropping
// external to money-only, matching local's single-value shape; the
// compressed-token count moves into the tooltip alongside the existing
// estimate disclosure, same pattern as local's hasTime/hasTokens split.
//
// Operator follow-up, 2026-08-06: the local chip's token count was noise —
// time saved is the number that actually matters, so it's now the only
// value shown. Same-day correction: the initial fix used a flat 50 tok/s
// "generation" fallback when no precise source resolved, which produced an
// implausible 493-hour "saved" figure inside a 168-hour window — Compressor's
// local benefit is PURELY avoided re-prefill, so the TPS basis must always
// be a real prefill measurement, never a guess. The fallback is deleted;
// compressor_summary_handlers.go's lookupPrefillTPS now prefers (in order) a
// profile depth-curve point, passively OBSERVED real throughput
// (internal/collector's OnPrefillSample, accumulated from ordinary
// traffic), a profile depth-0 scalar, a curated catalog row, or a live
// scrape — and the estimate is a SUM over the models that actually
// contributed cached requests, each priced against its own real TPS. A
// model with no resolvable TPS is omitted (logged server-side as an
// anomaly), which can legitimately leave this chip blank even when tokens
// were cached — see local.hasTime below.

function sumLocal(proxies: CompressorSummaryProxy[]) {
  let tokensSaved = 0;
  let timeSavedS = 0;
  let hasTokens = false;
  let hasTime = false;
  const sources = new Set<string>();
  for (const p of proxies) {
    if (p.kind !== "local") continue;
    if (p.tokens_saved_est != null) {
      tokensSaved += p.tokens_saved_est;
      hasTokens = true;
    }
    if (p.time_saved_seconds_est != null) {
      timeSavedS += p.time_saved_seconds_est;
      hasTime = true;
    }
    if (p.tps_source) sources.add(p.tps_source);
  }
  return { tokensSaved, hasTokens, timeSavedS, hasTime, sources };
}

function sumExternal(proxies: CompressorSummaryProxy[]) {
  // Compressor's own compression saving — tokens_saved priced at
  // compression_saved_display (already FX-converted server-side). Distinct
  // from (and not summed with) the provider's own prompt-cache discount,
  // which applies with or without Compressor in the request path and is
  // therefore not a Compressor saving — see estimateRemoteCompressionSaved in
  // compressor_summary_handlers.go.
  let compressedTokens = 0;
  let hasTokens = false;
  let moneyDisplay = 0;
  let hasMoney = false;
  for (const p of proxies) {
    if (p.kind !== "remote") continue;
    if (p.tokens_saved > 0) {
      compressedTokens += p.tokens_saved;
      hasTokens = true;
    }
    if (p.compression_saved_display != null) {
      moneyDisplay += p.compression_saved_display;
      hasMoney = true;
    }
  }
  return { compressedTokens, hasTokens, moneyDisplay, hasMoney };
}

export function CompressorSavingsChips({ window_ }: { window_: string }) {
  const compressorSummary = useCompressorSummary(window_);
  const proxies = compressorSummary.data?.proxies ?? [];
  const displayCurrency = compressorSummary.data?.display_currency ?? "USD";

  // The distinguishing word leads the label: .stat .k ellipsizes at narrow
  // widths (QA 2026-08-21 caught both chips truncating to the identical
  // "COMPRESSOR SAVE…"), so "local"/"external" must be visible before any
  // truncation can happen.
  if (compressorSummary.isLoading) {
    return (
      <>
        <div className="stat">
          <div className="k">{`Local · compressor saved (${window_})`}</div>
          <div className="v saved"></div>
        </div>
        <div className="stat">
          <div className="k">{`External · compressor saved (${window_})`}</div>
          <div className="v saved"></div>
        </div>
      </>
    );
  }

  const local = sumLocal(proxies);
  const external = sumExternal(proxies);

  const localTitle = !local.hasTokens
    ? "No cached local requests this window"
    : local.hasTime
      ? `${formatTokens(local.tokensSaved)} tokens not re-prefilled, estimated against real prefill TPS via ${Array.from(local.sources).join(", ")}`
      : "No model with cached requests this window had a real measured prefill TPS — time estimate unavailable";

  const externalTitle = !external.hasTokens
    ? "No compressed external requests this window"
    : external.hasMoney
      ? `${formatTokens(external.compressedTokens)} tokens compressed, priced at each provider's blended input rate in ${displayCurrency}`
      : `${formatTokens(external.compressedTokens)} tokens compressed, but no priced offering to value them against this window`;

  return (
    <>
      <div className="stat">
        <div className="k">{`Local · compressor saved (${window_})`}</div>
        <div className="v saved" title={localTitle}>
          {local.hasTime ? formatDurationShort(local.timeSavedS) : ""}
        </div>
      </div>
      <div className="stat">
        <div className="k">{`External · compressor saved (${window_})`}</div>
        <div className="v saved" title={externalTitle}>
          {external.hasMoney ? formatCurrencyPrecise(external.moneyDisplay, displayCurrency) : ""}
        </div>
      </div>
    </>
  );
}
