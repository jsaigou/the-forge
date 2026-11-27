// The order a new load takes the slots in (go/internal/engine/slot_order.go): a just-freed slot comes last, so a1 just unloaded means a2.
import type { Status } from "./types";

export function slotsInLoadOrder(status: Status): string[] {
  const all = Object.keys(status.slot_labels);
  const ordered = (status.load_order ?? []).filter((s) => all.includes(s));
  return [...ordered, ...all.filter((s) => !ordered.includes(s))];
}
