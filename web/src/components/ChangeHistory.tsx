import { useAuditLog } from "../lib/queries";

// ChangeHistory (Sprint C) — the read side of audit_log's new "why this
// change" reason field (catalog_handlers.go's withReason). Shared between
// ConfigDetailView and ModelDetailView; actionPrefix scopes to one entity
// kind since target ids collide across kinds (see AuditListResponse's doc
// comment, lib/types.ts).
export function ChangeHistory({ actionPrefix, target }: { actionPrefix: string; target: string }) {
  const { data, isLoading } = useAuditLog(actionPrefix, target, 20);

  if (isLoading) {
    return <div className="empty-note">Loading change history…</div>;
  }
  if (!data || data.entries.length === 0) {
    return <div className="empty-note">No recorded changes yet.</div>;
  }

  return (
    <div className="change-history">
      {data.entries.map((e) => (
        <div className="change-entry" key={e.id}>
          <div className="change-meta">
            <span className="who">{e.actor}</span>
            <span className="when">{new Date(e.ts).toLocaleString()}</span>
            <span className="what">{e.action.replace(/^catalog_(config|model)_/, "")}</span>
          </div>
          {e.detail && <div className="change-detail">{e.detail}</div>}
        </div>
      ))}
    </div>
  );
}
