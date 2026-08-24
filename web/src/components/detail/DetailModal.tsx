import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import type { ConfigCard, SchedulerStatus, Status } from "../../lib/types";
import { ConfigDetailView } from "./ConfigDetailView";
import { ConfigEditView } from "./ConfigEditView";
import { ModelBenchmarksView } from "./ModelBenchmarksView";
import { ModelEditView } from "./ModelEditView";
import { ModelHeroView } from "./ModelHeroView";

export type DetailViewEntry =
  | { kind: "config"; card: ConfigCard }
  | { kind: "model"; modelId: string; fromRect?: { x: number; y: number; width: number; height: number } }
  | { kind: "editConfig"; configId: number }
  | { kind: "editModel"; modelId: string }
  | { kind: "modelBenchmarks"; modelId: string };

// DetailModal — the shared expanded-detail view stack (Sprint B). Opens on
// either a config (from ConfigCardView) or a model (from the portrait
// ModelCardView); "View model →" from within a config pushes the model
// view in place rather than jumping tabs, so an operator drilling into a
// model's other configs never loses the config they started from. Esc and
// the backdrop close the whole stack, matching every other modal in the
// app (.modal-backdrop's existing convention) rather than only popping one
// level.
export function DetailModal({
  initial,
  status,
  schedulerStatus,
  displayCurrency = "USD",
  onClose,
}: {
  initial: DetailViewEntry;
  status: Status;
  schedulerStatus: SchedulerStatus | undefined;
  displayCurrency?: string;
  onClose: () => void;
}) {
  const [stack, setStack] = useState<DetailViewEntry[]>([initial]);
  const current = stack[stack.length - 1];

  // Operator feedback 2026-08-14: tapping outside the card (or Esc / ↑) goes
  // back ONE level — pop the view stack first, close only at the root.
  function backOneLevel() {
    if (stack.length > 1) setStack((s) => s.slice(0, -1));
    else onClose();
  }

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape" || e.key === "ArrowUp") {
        e.preventDefault();
        backOneLevel();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  function pushModel(modelId: string) {
    setStack((s) => [...s, { kind: "model", modelId }]);
  }
  function pushEditConfig(configId: number) {
    setStack((s) => [...s, { kind: "editConfig", configId }]);
  }
  function pushEditModel(modelId: string) {
    setStack((s) => [...s, { kind: "editModel", modelId }]);
  }
  function pushModelBenchmarks(modelId: string) {
    setStack((s) => [...s, { kind: "modelBenchmarks", modelId }]);
  }
  function back() {
    setStack((s) => (s.length > 1 ? s.slice(0, -1) : s));
  }

  // createPortal to document.body: the .modal-backdrop is position:fixed, and
  // any transformed ancestor (the model carousel's .umc-track uses a transform
  // for momentum sliding) would otherwise become the containing block and trap
  // the modal inside the carousel's viewport. Operator feedback 2026-08-14 #2:
  // "carousel does not work on desktop" traced to exactly this.
  return createPortal(
    <div
      className="modal-backdrop"
      onClick={(e) => {
        // Sprint I fix: without stopPropagation, this click bubbles past the
        // backdrop into whatever clickable ancestor rendered the modal (e.g.
        // ModelCardView/ConfigCardView's own onClick={openDetail}), which
        // re-opens it in the same batched React update — the modal appeared
        // to never close on an outside click. See
        // docs/v5-prerelease-readiness.md's Sprint I card-close writeup.
        e.stopPropagation();
        backOneLevel();
      }}
    >
      {/* Playing-card redesign 2026-08-14: the model view is the zoomed
          playing-card hero (compact UI, flip for change history) instead of
          the old detail-page panel — it brings its own floating bar
          buttons, so it renders WITHOUT the .modal.wide chrome. Pushed
          sub-views (edit/benchmarks/config) still use the panel. */}
      {current.kind === "model" ? (
        <ModelHeroView
          modelId={current.modelId}
          fromRect={current.fromRect}
          status={status}
          displayCurrency={displayCurrency}
          canGoBack={stack.length > 1}
          onBack={back}
          onClose={onClose}
          onEdit={() => pushEditModel(current.modelId)}
          onEditBenchmarks={() => pushModelBenchmarks(current.modelId)}
        />
      ) : (
        <div className="modal wide detail-modal" onClick={(e) => e.stopPropagation()}>
          <div className="detail-modal-bar">
            {stack.length > 1 ? (
              <button className="icon-btn" title="Back" aria-label="Back" onClick={back}>←</button>
            ) : (
              <span />
            )}
            <button className="icon-btn" title="Close" aria-label="Close" onClick={onClose}>✕</button>
          </div>
          {current.kind === "config" ? (
            <ConfigDetailView
              card={current.card}
              status={status}
              schedulerStatus={schedulerStatus}
              displayCurrency={displayCurrency}
              onViewModel={pushModel}
              onEdit={() => pushEditConfig(current.card.id)}
            />
          ) : current.kind === "editConfig" ? (
            <ConfigEditView configId={current.configId} onDone={back} onCancel={back} />
          ) : current.kind === "editModel" ? (
            <ModelEditView modelId={current.modelId} onDone={back} onCancel={back} />
          ) : (
            <ModelBenchmarksView modelId={current.modelId} />
          )}
        </div>
      )}
    </div>,
    document.body,
  );
}
