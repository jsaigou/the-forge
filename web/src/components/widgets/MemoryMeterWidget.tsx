import { formatGB } from "../../lib/format";
import { useConfigCards, useSchedulerStatus, useStatus } from "../../lib/queries";

// Widget "memory-meter" (see lib/widgetRegistry.ts). Phase 5 (2026-08-12)
// relocated it to the Resources tab; S2 (2026-08-23) added attribution —
// the bar now names every GTT holder (slots, ComfyUI, always-on services)
// and carries an honestly-labelled unattributed remainder instead of
// silently folding non-slot consumers into "free" (the operator complaint:
// ~26 GB of GTT held by an idle ComfyUI read as free).

function friendlyUnit(unit: string): string {
  const known: Record<string, string> = {
    "ai-mode-comfyui": "ComfyUI",
    "forge-embedding": "Embedding",
    "forge-stt": "Speech-to-text",
    "forge-tts": "TTS",
    "forge-aligner": "Aligner",
    forge: "forge",
  };
  if (known[unit]) return known[unit];
  const compress = unit.match(/^forge-compress@(.+)$/);
  if (compress) return `Compressor · ${compress[1]}`;
  return unit;
}

export function MemoryMeterWidget() {
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
    .map(([unit, bytes]) => ({ unit, name: friendlyUnit(unit), bytes }))
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
    segments.push({ cls: "other", pct: (otherBytes / totalGtt) * 100, name: "other", detail: null });
  }

  const committedBytes = allBytesKnown ? slotTotal + otherBytes : usedFloor;
  const committedPct = totalGtt > 0 ? Math.min(100, (committedBytes / totalGtt) * 100) : 0;
  const freePct = Math.max(0, 100 - committedPct);
  const freeBytes = Math.max(0, totalGtt - committedBytes);

  // Legend rows: everything the bar names, with figures — this is where
  // ComfyUI gets named even at widths where its in-bar label truncates.
  const legend: { color: string; name: string; bytes: string }[] = [];
  for (let i = 0; i < loadedSlots.length; i++) {
    const s = loadedSlots[i];
    legend.push({
      color: `var(--${segClasses[i % 4]})`,
      name: `${s.label} · ${s.mode}${s.measured ? "" : " (~est)"}`,
      bytes: s.bytes != null ? `${formatGB(s.bytes)} GB` : "unknown",
    });
  }
  for (const h of auxHolders) {
    legend.push({ color: "var(--m2)", name: h.name, bytes: `${formatGB(h.bytes)} GB` });
  }
  if (otherBytes > 0) {
    legend.push({ color: "var(--text-mute)", name: "Other (kernel / driver / untracked)", bytes: `${formatGB(otherBytes)} GB` });
  }

  return (
    <>
      <div className="eyebrow">Memory usage · {budget ? `${formatGB(budget.total_bytes)} GB unified GTT` : "unified GTT"}</div>
      <div className="budget">
        {budget ? (
          <div className="meter">
            {segments.map((seg, i) => {
              // Bigger in-bar text needs more room; degrade gracefully
              // rather than clip a half-drawn label.
              const text = seg.pct >= 20 ? [seg.name, seg.detail].filter(Boolean).join(" · ") : seg.pct >= 10 ? seg.name : null;
              return (
                <div className={`seg ${seg.cls}`} key={`${seg.cls}-${i}`} title={seg.name ?? undefined} style={{ width: `${seg.pct}%` }}>
                  {text ?? ""}
                </div>
              );
            })}
            <div className="seg free" style={{ width: `${freePct}%` }}>
              {freePct >= 10 ? `${formatGB(freeBytes)} GB free` : ""}
            </div>
          </div>
        ) : (
          <div className="empty-note">Loading memory budget…</div>
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
