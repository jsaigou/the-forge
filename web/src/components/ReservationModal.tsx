import { useState } from "react";
import { apiErrorMessage } from "../lib/api";
import { useCreateReservation } from "../lib/queries";
import type { Status } from "../lib/types";

const SLOT_OPTIONS = ["a1", "a2", "a3", "a4"];

export function ReservationModal({ status, onClose }: { status: Status; onClose: () => void }) {
  const create = useCreateReservation();
  const [label, setLabel] = useState("");
  const [model, setModel] = useState("");
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const [scope, setScope] = useState<"bay" | "whole_box" | "comfyui">("bay");
  const [bay, setBay] = useState(SLOT_OPTIONS[0]);
  const [error, setError] = useState<string | null>(null);

  const modeOptions = Object.entries(status.modes_available).filter(([, m]) => m.slot_capable);

  async function submit() {
    setError(null);
    if (!label || !model || !start || !end) {
      setError("Label, model, start, and end are required.");
      return;
    }
    try {
      await create.mutateAsync({
        label,
        model,
        start: new Date(start).toISOString(),
        end: new Date(end).toISOString(),
        scope,
        bay: scope === "bay" ? bay : null,
        created_by: "human",
      });
      onClose();
    } catch (e) {
      setError(apiErrorMessage(e));
    }
  }

  return (
    <div
      className="modal-backdrop"
      onClick={(e) => {
        // Sprint I fix — see DetailModal.tsx's identical comment. Not
        // nested in a clickable ancestor today, but the guard is free and
        // prevents a future regression if this modal's trigger ever moves.
        e.stopPropagation();
        onClose();
      }}
    >
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>New reservation</h3>
        <div className="form-grid">
          <label className="form-row">
            Label
            <input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="nightly eval" />
          </label>
          <label className="form-row">
            Model
            <select value={model} onChange={(e) => setModel(e.target.value)}>
              <option value="">select…</option>
              {modeOptions.map(([key, m]) => (
                <option key={key} value={key}>{m.label}</option>
              ))}
            </select>
          </label>
          <label className="form-row">
            Start
            <input type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} />
          </label>
          <label className="form-row">
            End
            <input type="datetime-local" value={end} onChange={(e) => setEnd(e.target.value)} />
          </label>
          <label className="form-row">
            Scope
            <select value={scope} onChange={(e) => setScope(e.target.value as typeof scope)}>
              <option value="bay">Bay hold</option>
              <option value="whole_box">Whole box</option>
              <option value="comfyui">ComfyUI</option>
            </select>
          </label>
          {scope === "bay" && (
            <label className="form-row">
              Bay
              <select value={bay} onChange={(e) => setBay(e.target.value)}>
                {SLOT_OPTIONS.map((s) => (
                  <option key={s} value={s}>{status.slot_labels[s] ?? s}</option>
                ))}
              </select>
            </label>
          )}
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-actions">
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn primary" disabled={create.isPending} onClick={submit}>
            {create.isPending ? "Creating…" : "Create"}
          </button>
        </div>
      </div>
    </div>
  );
}
