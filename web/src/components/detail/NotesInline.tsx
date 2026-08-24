import { useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import { useCatalogNotes, useCreateCatalogNote, useDeleteCatalogNote, useUpdateCatalogNote } from "../../lib/queries";
import { useSession } from "../../lib/session";
import type { CatalogNote } from "../../lib/types";
import { ConfirmButton } from "../ConfirmButton";
import { StepUpModal } from "../StepUpModal";
import { useStepUpGate } from "../../lib/useStepUpGate";

// NotesInline — Phase 8 (pre-release feedback sprint). Notes were curated
// operator annotations with full backend CRUD (including a PUT the FE never
// called) but exactly one call site anywhere in the app: the global,
// unscoped Settings -> Catalog -> Notes tab. Production has ~0 rows because
// there was nowhere to read or write one in context. This surfaces them
// inline on ModelDetailView/ConfigDetailView via the same server-side
// subject filter CatalogPanel's NotesSection already had
// (useCatalogNotes(subjectType, subjectId)) — no new backend surface.
//
// Unlike NotesSection's form (subject_type + subject select + free-text
// author), the subject is already known from context here, and author comes
// from the logged-in session rather than a free-text box — author is
// server-required, and a human typing their own name is a worse record than
// the session already has.
export function NotesInline({ subjectType, subjectId }: { subjectType: "model" | "config"; subjectId: number }) {
  const { canAdmin, username } = useSession();
  const notes = useCatalogNotes(subjectType, subjectId);
  const create = useCreateCatalogNote();
  const update = useUpdateCatalogNote();
  const remove = useDeleteCatalogNote();
  const gate = useStepUpGate();

  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState("");
  const [editingId, setEditingId] = useState<number | null>(null);
  const [editDraft, setEditDraft] = useState("");
  const [error, setError] = useState<string | null>(null);

  const list = notes.data ?? [];

  function submitCreate() {
    setError(null);
    const doCreate = () =>
      create.mutate(
        { subject_type: subjectType, subject_id: subjectId, author: username, body: draft },
        {
          onSuccess: () => { setAdding(false); setDraft(""); },
          onError: (e) => { if (!gate.handle(e, doCreate)) setError(apiErrorMessage(e)); },
        },
      );
    doCreate();
  }

  function submitEdit(n: CatalogNote) {
    setError(null);
    // Named fields only, not a spread of the full CatalogNote — the PUT
    // body's Go struct (noteBody) has exactly subject_type/subject_id/
    // author/body and rejects unknown fields (DisallowUnknownFields), so
    // including id/created_at/updated_at 400s. Same class of bug as the
    // Phase 7 offering-routing PATCH fix; see
    // feedback_fullreplace_curl_verification_hazard in project memory.
    const doUpdate = () =>
      update.mutate(
        { id: n.id, n: { subject_type: n.subject_type, subject_id: n.subject_id, author: n.author, body: editDraft } },
        {
          onSuccess: () => setEditingId(null),
          onError: (e) => { if (!gate.handle(e, doUpdate)) setError(apiErrorMessage(e)); },
        },
      );
    doUpdate();
  }

  function submitDelete(id: number) {
    setError(null);
    const doDelete = () =>
      remove.mutate(id, {
        onError: (e) => { if (!gate.handle(e, doDelete)) setError(apiErrorMessage(e)); },
      });
    doDelete();
  }

  if (!canAdmin && list.length === 0) return null;

  return (
    <div style={{ marginTop: 16 }}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <div className="eyebrow" style={{ fontSize: 11 }}>Notes</div>
        {canAdmin && !adding && (
          <button className="icon-btn" style={{ fontSize: 11, padding: "2px 8px" }} onClick={() => { setAdding(true); setDraft(""); setError(null); }}>
            + Add
          </button>
        )}
      </div>

      {error && <div className="error-note" style={{ marginTop: 6 }}>{error}</div>}

      {notes.isError ? (
        <div className="empty-note" style={{ marginTop: 6 }}>Catalog not available (503 — store may not be wired).</div>
      ) : list.length === 0 && !adding ? (
        <div className="empty-note" style={{ marginTop: 6 }}>No notes yet.</div>
      ) : (
        <div style={{ marginTop: 6, display: "flex", flexDirection: "column", gap: 8 }}>
          {list.map((n) => (
            <div key={n.id} className="qrow" style={{ flexDirection: "column", alignItems: "stretch", gap: 4 }}>
              {editingId === n.id ? (
                <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                  <textarea
                    value={editDraft}
                    rows={3}
                    style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 12, resize: "vertical" }}
                    onChange={(e) => setEditDraft(e.target.value)}
                  />
                  <div className="form-actions">
                    <button className="btn" onClick={() => setEditingId(null)}>Cancel</button>
                    <button className="btn primary" disabled={update.isPending || !editDraft} onClick={() => submitEdit(n)}>Save</button>
                  </div>
                </div>
              ) : (
                <>
                  <div style={{ fontSize: 12, color: "var(--text-dim)", whiteSpace: "pre-wrap" }}>{n.body}</div>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
                      {n.author} · {new Date(n.updated_at).toLocaleDateString()}
                    </span>
                    {canAdmin && (
                      <div style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                        <button
                          className="btn"
                          style={{ fontSize: 10.5, padding: "2px 6px" }}
                          onClick={() => { setEditingId(n.id); setEditDraft(n.body); setError(null); }}
                        >
                          Edit
                        </button>
                        <ConfirmButton
                          className="btn"
                          style={{ fontSize: 10.5, padding: "2px 6px" }}
                          pending={remove.isPending}
                          onConfirm={() => submitDelete(n.id)}
                          warning="Delete this note?"
                        />
                      </div>
                    )}
                  </div>
                </>
              )}
            </div>
          ))}
        </div>
      )}

      {adding && (
        <div style={{ marginTop: 8, display: "flex", flexDirection: "column", gap: 6 }}>
          <textarea
            value={draft}
            rows={3}
            autoFocus
            placeholder="What should the next operator know?"
            style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 12, resize: "vertical" }}
            onChange={(e) => setDraft(e.target.value)}
          />
          <div className="form-actions">
            <button className="btn" onClick={() => { setAdding(false); setError(null); }}>Cancel</button>
            <button className="btn primary" disabled={create.isPending || !draft} onClick={submitCreate}>Add note</button>
          </div>
        </div>
      )}

      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </div>
  );
}
