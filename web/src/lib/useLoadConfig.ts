import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError, apiErrorMessage } from "./api";
import { formatGB } from "./format";
import { qk, useLoadModel, useUnloadSlot } from "./queries";
import type { ConfigCard, Status } from "./types";

export type ConfigLoadState = "idle" | "loading" | "loaded" | "evict-needed";

// deriveConfigState was duplicated verbatim as ConfigCardView's
// deriveCardState (matching by the full card) and ModelCardView's
// deriveRowState (matching by a bare config name) — the two were never
// actually different, since a ConfigCard's identity for slot-matching
// purposes is just its name (the mode name). One copy, keyed by name.
export function deriveConfigState(
  configName: string,
  status: Status,
): { state: ConfigLoadState; slot: string | null } {
  const slotKeys = Object.keys(status.slot_labels);

  for (const s of slotKeys) {
    if (status.slots[s] === configName) return { state: "loaded", slot: s };
  }
  for (const s of slotKeys) {
    const sl = status.slot_loading[s];
    if (sl?.in_progress && sl.mode === configName) return { state: "loading", slot: s };
  }
  const hasEmpty = slotKeys.some(
    (s) => !status.slots[s] && !status.slot_loading[s]?.in_progress && !status.slot_unloading[s]?.in_progress,
  );
  return { state: hasEmpty ? "idle" : "evict-needed", slot: null };
}

interface WontFitBody {
  error?: string;
  slot?: string;
  message?: string;
  required_bytes?: number;
  free_bytes?: number;
}

export interface UseLoadConfigResult {
  state: ConfigLoadState;
  activeSlot: string | null;
  slotKeys: string[];
  emptySlot: string | undefined;
  confirming: boolean;
  evictTarget: string;
  setEvictTarget: (slot: string) => void;
  loadError: string | null;
  busy: boolean;
  openConfirm: () => void;
  closeConfirm: () => void;
  doLoad: () => Promise<void>;
}

// useLoadConfig owns the whole load/evict/409-handling flow for a single
// config — previously duplicated near-verbatim between ConfigCardView.tsx
// and ModelCardView.tsx's ConfigRow (Sprint B dedup).
export function useLoadConfig(card: ConfigCard, status: Status): UseLoadConfigResult {
  const load = useLoadModel();
  const unload = useUnloadSlot();
  const qc = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const [evictTarget, setEvictTarget] = useState("");
  const [loadError, setLoadError] = useState<string | null>(null);

  const slotKeys = Object.keys(status.slot_labels);
  const { state, slot: activeSlot } = deriveConfigState(card.name, status);
  const emptySlot = slotKeys.find(
    (s) => !status.slots[s] && !status.slot_loading[s]?.in_progress && !status.slot_unloading[s]?.in_progress,
  );
  const busy = load.isPending || unload.isPending;

  function openConfirm() {
    setLoadError(null);
    setEvictTarget("");
    setConfirming(true);
  }
  function closeConfirm() {
    if (!busy) setConfirming(false);
  }

  async function doLoad() {
    const slot = state === "evict-needed" ? evictTarget : emptySlot;
    if (!slot) {
      setLoadError("No slot available. Close and try again.");
      return;
    }
    setLoadError(null);
    try {
      if (state === "evict-needed") await unload.mutateAsync(slot);
      await load.mutateAsync({ mode: card.name, slot });
      setConfirming(false);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        const body = e.body as WontFitBody;
        if (body.error === "already_loaded") {
          qc.invalidateQueries({ queryKey: qk.status });
          qc.invalidateQueries({ queryKey: qk.schedulerStatus });
          setLoadError(body.message ?? `Already loaded on slot ${body.slot ?? "?"}`);
        } else if (body.error === "wont_fit") {
          const need = body.required_bytes != null ? formatGB(body.required_bytes) : null;
          const free = body.free_bytes != null ? formatGB(body.free_bytes) : null;
          const sizes = need != null && free != null ? ` (needs ${need} GB, ${free} GB free)` : "";
          setLoadError(`Won't fit${sizes}. ${body.message ?? ""}`.trim());
        } else {
          setLoadError(body.message ?? apiErrorMessage(e));
        }
      } else {
        setLoadError(apiErrorMessage(e));
      }
    }
  }

  return {
    state, activeSlot, slotKeys, emptySlot,
    confirming, evictTarget, setEvictTarget, loadError, busy,
    openConfirm, closeConfirm, doLoad,
  };
}
