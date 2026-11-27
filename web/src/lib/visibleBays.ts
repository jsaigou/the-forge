// Which bays the Console shows: every bay that has something to say, plus at most one free bay to load into (design feedback 2026-11-27 —
// reverses the 2026-08-06 "always all four").
import { slotsInLoadOrder } from "./loadOrder";
import type { Reservation, Status } from "./types";

export function visibleBays(slotKeys: string[], status: Status, reservations: Reservation[] | undefined, now = Date.now()): string[] {
  const reserved = new Set(
    (reservations ?? []).filter((r) => r.scope === "bay" && new Date(r.end).getTime() > now).map((r) => r.bay),
  );
  const failed = (k: string) => status.slot_states?.[k]?.state === "failed";
  const inUse = (k: string) =>
    !!status.slots[k] || !!status.slot_loading[k]?.in_progress || !!status.slot_unloading[k]?.in_progress || reserved.has(k) || failed(k);
  // A bay whose last load failed is free as well as noteworthy, so it already is the free bay on offer.
  const offersFree = slotKeys.some((k) => failed(k) && !status.slots[k]);
  // The free bay on offer is the one a load would take, which is not necessarily the first (lib/loadOrder.ts).
  const nextFree = offersFree ? undefined : slotsInLoadOrder(status).find((k) => slotKeys.includes(k) && !inUse(k));
  return slotKeys.filter((k) => inUse(k) || k === nextFree);
}
