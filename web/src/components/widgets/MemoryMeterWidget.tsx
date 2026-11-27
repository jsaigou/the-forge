import { useTranslation } from "react-i18next";
import { formatGB } from "../../lib/format";
import { useConfigCards, useSchedulerStatus, useStatus } from "../../lib/queries";

// Widget "memory-meter" (see lib/widgetRegistry.ts). Phase 5 (2026-08-12)
// relocated it to the Resources tab; S2 (2026-08-23) added attribution —
// the bar now names every GTT holder (slots, ComfyUI, always-on services)
// and carries an honestly-labelled unattributed remainder instead of
// silently folding non-slot consumers into "free" (the operator complaint:
// ~26 GB of GTT held by an idle ComfyUI read as free).

function friendlyUnit(unit: string, t: (key: string, opts?: Record<string, unknown>) => string): string {
  const known: Record<string, string> = {
    "ai-mode-comfyui": t("memory_meter.unit_comfyui"),
    "forge-embedding": t("memory_meter.unit_embedding"),
    "forge-stt": t("memory_meter.unit_stt"),
    "forge-tts": t("memory_meter.unit_tts"),
    "forge-aligner": t("memory_meter.unit_aligner"),
    forge: t("memory_meter.unit_forge"),
  };
  if (known[unit]) return known[unit];
  const pool = unit.match(/^forge-pool@(.+)$/);
  if (pool) return t("memory_meter.unit_pool", { slot: pool[1].toUpperCase() });
  const compress = unit.match(/^forge-compress@(.+)$/);
  if (compress) return t("memory_meter.unit_compressor", { proxy: compress[1] });
  return unit;
}

export function MemoryMeterWidget() {
  const { t } = useTranslation("dashboard");
  const status = useStatus();
  const schedulerStatus = useSchedulerStatus();
  const configCards = useConfigCards("7d");

  const budget = schedulerStatus.data?.memory_budget;
  const totalGtt = budget?.total_bytes ?? 0;

  const loadedSlots = status.data
    ? Object.keys(status.data.slot_labels)
        .filter((s) => status.data!.slots[s])
        .map((s) => {
          const liveBytes = schedulerStatus.data?.slot_memory_bytes?.[s];
          const card = configCards.data?.cards.find((c) => c.name === status.data!.slots[s]!);
          return {
            slot: s,
            label: status.data!.slot_labels[s],
            mode: status.data!.slots[s]!,
            card,
            // Real, live GPU footprint (VRAM+GTT via fdinfo) when the
            // collector could read it — includes KV cache. Falls back to
            // the curated weights-only catalog estimate only when the live
            // figure is unavailable (e.g. non-amdgpu, probe miss).
            bytes: liveBytes && liveBytes > 0 ? liveBytes : card?.derived.memory_req_bytes ?? null,
            measured: liveBytes != null && liveBytes > 0,
          };
        })
    : [];
  // Split the committed segment into one .seg.m{n} per loaded slot (a
  // distinct categorical color per model), sized by its real or estimated
  // memory footprint — only when every loaded slot's figure is known;
  // otherwise fall back to a single blob (guessing a split from partial
  // data would misrepresent which slot holds how much).
  const segClasses = ["m1", "m2", "m3", "m4"];
  const allBytesKnown = loadedSlots.length > 0 && loadedSlots.every((s) => s.bytes != null);
  const slotTotal = allBytesKnown ? loadedSlots.reduce((n, s) => n + s.bytes!, 0) : 0;

  // S2 attribution: named non-slot holders, largest first.
  const auxHolders = Object.entries(schedulerStatus.data?.unit_memory_bytes ?? {})
    .filter(([, b]) => b > 0)
    .map(([unit, bytes]) => ({ unit, name: friendlyUnit(unit, t), bytes }))
    .sort((a, b) => b.bytes - a.bytes || a.unit.localeCompare(b.unit));
  const auxTotal = auxHolders.reduce((n, h) => n + h.bytes, 0);

  // The budget's used_bytes is the deliberately conservative OOM-prevention
  // figure (gtt_used + rocm-slot RSS — see CLAUDE.md's "GTT counter blind
  // spot"), not a display number. Whatever it says is used beyond what we
  // can NAME becomes the honest "other" segment: kernel/driver/untracked.
  const usedFloor = budget?.used_bytes ?? 0;
  const otherBytes = Math.max(0, usedFloor - slotTotal - auxTotal);

  type Seg = { cls: string; pct: number; name: string | null; detail: string | null };
  const segments: Seg[] = [];
  if (allBytesKnown && totalGtt > 0) {
    for (let i = 0; i < loadedSlots.length; i++) {
      const s = loadedSlots[i];
      segments.push({
        cls: segClasses[i % 4],
        pct: (s.bytes! / totalGtt) * 100,
        name: s.mode,
        detail: `${s.measured ? "" : "~"}${formatGB(s.bytes!)} GB`,
      });
    }
  } else if ((budget?.used_bytes ?? 0) > 0) {
    // Old fallback shape: no per-slot split without full data. Slots' share
    // shows as the plain committed blob; attribution below still lists what
    // IS known.
    segments.push({ cls: "m1", pct: (usedFloor / totalGtt) * 100, name: null, detail: null });
  }
  for (const h of auxHolders) {
    if (totalGtt <= 0) break;
    segments.push({ cls: "aux", pct: (h.bytes / totalGtt) * 100, name: h.name, detail: `${formatGB(h.bytes)} GB` });
  }
  if (otherBytes > 0 && totalGtt > 0 && (allBytesKnown || auxHolders.length > 0)) {
    segments.push({ cls: "other", pct: (otherBytes / totalGtt) * 100, name: t("memory_meter.other"), detail: null });
  }

  let committedBytes = allBytesKnown ? slotTotal + otherBytes : usedFloor;
  let committedPct = totalGtt > 0 ? Math.min(100, (committedBytes / totalGtt) * 100) : 0;
  let freePct = Math.max(0, 100 - committedPct);
  let freeBytes = Math.max(0, totalGtt - committedBytes);

  // Host-memory partition (internal/memledger). On this machine GTT pages are system RAM, so the bar is drawn over the HOST total and split into
  // regions: what the models hold, what a new load may still claim, what is held back (host floor), and what is reserved up front for the declared
  // always-on specialists, the OS and the growth margin. Falls back to the GTT-ceiling bar above when the daemon predates the partition.
  const part = budget?.partition;
  const reserveRows: { name: string; bytes: number }[] = [];
  if (part && part.host_total_bytes > 0) {
    const host = part.host_total_bytes;
    const pct = (b: number) => (Math.max(0, b) / host) * 100;
    segments.length = 0;
    let slotsShown = 0;
    if (allBytesKnown) {
      for (let i = 0; i < loadedSlots.length; i++) {
        const sl = loadedSlots[i];
        slotsShown += sl.bytes!;
        segments.push({ cls: segClasses[i % 4], pct: pct(sl.bytes!), name: sl.mode, detail: `${sl.measured ? "" : "~"}${formatGB(sl.bytes!)} GB` });
      }
    }
    const modelOther = Math.max(0, part.model_used_bytes - slotsShown);
    if (modelOther > 0) segments.push({ cls: allBytesKnown ? "other" : "m1", pct: pct(modelOther), name: t("memory_meter.other_models"), detail: null });
    const held = Math.max(0, part.model_budget_bytes - part.model_used_bytes - part.free_bytes);
    reserveRows.push({ name: t("memory_meter.reserved_specialists"), bytes: part.specialist_declared_bytes });
    reserveRows.push({ name: t("memory_meter.reserved_os"), bytes: part.os_floor_bytes });
    reserveRows.push({ name: t("memory_meter.reserved_margin"), bytes: part.growth_margin_bytes });
    committedBytes = part.model_used_bytes;
    freeBytes = part.free_bytes;
    freePct = pct(freeBytes);
    // Free sits right after the models; the remaining regions follow in a fixed order so the eye can read the bar left to right.
    segments.push({ cls: "free", pct: freePct, name: t("memory_meter.free", { amount: formatGB(freeBytes) }), detail: null });
    if (held > 0) segments.push({ cls: "held", pct: pct(held), name: t("memory_meter.held_by", { bound: t(`memory_meter.bound_${part.binding}`) }), detail: null });
    for (const r of reserveRows) if (r.bytes > 0) segments.push({ cls: "reserve", pct: pct(r.bytes), name: r.name, detail: `${formatGB(r.bytes)} GB` });
  }

  // Legend rows: everything the bar names, with figures — this is where
  // ComfyUI gets named even at widths where its in-bar label truncates.
  const legend: { color: string; name: string; bytes: string }[] = [];
  for (let i = 0; i < loadedSlots.length; i++) {
    const s = loadedSlots[i];
    legend.push({
      color: `var(--${segClasses[i % 4]})`,
      name: `${s.label} · ${s.mode}${s.measured ? "" : t("memory_meter.est_suffix")}`,
      bytes: s.bytes != null ? `${formatGB(s.bytes)} GB` : t("memory_meter.unknown_bytes"),
    });
  }
  for (const h of auxHolders) {
    legend.push({ color: "var(--m2)", name: h.name, bytes: `${formatGB(h.bytes)} GB` });
  }
  for (const r of reserveRows) {
    if (r.bytes > 0) legend.push({ color: "var(--text-mute)", name: r.name, bytes: `${formatGB(r.bytes)} GB` });
  }
  if (part && part.swap_total_bytes > 0 && part.swap_used_bytes > 0) {
    legend.push({ color: "var(--warn)", name: t("memory_meter.swap_in_use"), bytes: `${formatGB(part.swap_used_bytes)} / ${formatGB(part.swap_total_bytes)} GB` });
  }
  if (otherBytes > 0 && !part) {
    legend.push({ color: "var(--text-mute)", name: t("memory_meter.other_legend"), bytes: `${formatGB(otherBytes)} GB` });
  }
  if (part && part.host_total_bytes > 0) {
    const slotsShown = allBytesKnown ? loadedSlots.reduce((n, sl) => n + sl.bytes!, 0) : 0;
    const modelOther = Math.max(0, part.model_used_bytes - slotsShown);
    if (modelOther > 0) legend.push({ color: "var(--text-mute)", name: t("memory_meter.other_models_legend"), bytes: `${formatGB(modelOther)} GB` });
    legend.push({ color: "var(--ok)", name: t("memory_meter.free_legend", { bound: t(`memory_meter.bound_${part.binding}`) }), bytes: `${formatGB(part.free_bytes)} GB` });
  }

  return (
    <>
      <div className="eyebrow">
        {part && part.host_total_bytes > 0
          ? t("memory_meter.title_host", { total: formatGB(part.host_total_bytes) })
          : budget
            ? t("memory_meter.title_with_total", { total: formatGB(budget.total_bytes) })
            : t("memory_meter.title_no_total")}
      </div>
      <div className="budget">
        {budget ? (
          <div className="meter">
            {segments.map((seg, i) => {
              // Bigger in-bar text needs more room; degrade gracefully
              // rather than clip a half-drawn label.
              const quiet = seg.cls === "reserve" || seg.cls === "held";
              const text = quiet ? null : seg.pct >= 20 ? [seg.name, seg.detail].filter(Boolean).join(" · ") : seg.pct >= 10 ? seg.name : null;
              return (
                <div className={`seg ${seg.cls}`} key={`${seg.cls}-${i}`} title={seg.name ?? undefined} style={{ width: `${seg.pct}%` }}>
                  {text ?? ""}
                </div>
              );
            })}
            {!part && (
              <div className="seg free" style={{ width: `${freePct}%` }}>
                {freePct >= 10 ? t("memory_meter.free", { amount: formatGB(freeBytes) }) : ""}
              </div>
            )}
          </div>
        ) : (
          <div className="empty-note">{t("memory_meter.loading")}</div>
        )}
        {legend.length > 0 && (
          <div className="memlegend">
            {legend.map((row) => (
              <div className="row" key={row.name}>
                <span className="dot" style={{ background: row.color }} />
                <span className="name">{row.name}</span>
                <span className="bytes">{row.bytes}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  );
}
