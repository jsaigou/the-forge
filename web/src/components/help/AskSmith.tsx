import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { apiErrorMessage } from "../../lib/api";
import {
  useSmithAction,
  useSmithActions,
  useSmithChat,
  useSmithChecks,
  useSmithChecksRun,
  useSmithConversation,
  useSmithConversationDelete,
  useSmithConversations,
  useSmithPendingAsk,
  useSmithStatus,
  useSmithStreamingText,
  useSmithToolActivity,
  useSmithTurnStatus,
} from "../../lib/queries";
import type { SmithCheckMeta, SmithMessage, SmithRunbookStep, SmithToolCallEvidence } from "../../lib/types";
import { ConfirmButton } from "../ConfirmButton";
import { HammerIcon } from "../icons/HammerIcon";
import { ActionCard } from "../smith/ActionCard";
import { DigDeeperChip, stripDigDeeper } from "../smith/DigDeeperChip";
import { EvidenceBlock, parseEvidence } from "../smith/EvidenceBlock";
import { Markdown } from "../smith/Markdown";
import { RunbookCard } from "../smith/RunbookCard";
import { SmithIndicators } from "../smith/SelfContextChip";
import { SourcesList } from "../smith/SourcesList";
import { ToolActivityList, ToolCallCard } from "../smith/ToolCallCard";
import { SweepResult } from "./Diagnostics";

// Ask the smith: the P3 chat sub-tab (docs/v5-smith.md §6). Streaming rides
// the existing single app-wide EventSource (lib/sse.ts): smith:token
// deltas land in a client-only cache slot (qk.smith.streaming, read via
// useSmithStreamingText) keyed by the placeholder assistant message ID
// POST /chat returns; smith:message_done invalidates the conversation so
// the persisted row (real content, model, tier) takes over from the
// streamed text; that handoff is what MessageBubble's `hasContent` check
// below implements.

function parseConvId(sub?: string): number | null {
  const m = sub?.match(/conv\/(\d+)/);
  return m ? parseInt(m[1], 10) : null;
}

// Suggested questions shown in the collapsible panel attached to the chat
// input. Clicking one sends it immediately (no separate "press Send" step).
const QUICK_QUESTIONS = [
  "Is ComfyUI healthy?",
  "How much memory is free?",
  "What services are degraded?",
  "Is A0 working?",
  "What's in the backlog?",
  "Is llama.cpp up to date?",
  "Are there any pending investigation tasks?",
];

// LiveActionCard (Sprint S3-Web §2.4.2): action-kind messages carry
// {"action_id": N} evidence (NOT a serialized action; it would go stale
// through pending→executing→done). This component fetches the live action
// via GET /actions/{id} and re-renders on smith:action_update SSE. Falls
// through to a plain notice if the action can't be loaded (deleted, store
// unwired) so the transcript never has a blank hole.
function LiveActionCard({ actionId }: { actionId: number }) {
  const action = useSmithAction(actionId);
  if (action.isLoading) {
    return (
      <div className="smith-msg smith-msg-assistant" style={{ maxWidth: "100%" }}>
        <div className="card" style={{ padding: "10px 14px" }}>
          <span style={{ color: "var(--text-mute)" }}>Loading action…</span>
        </div>
      </div>
    );
  }
  if (action.isError || !action.data) {
    return (
      <div className="smith-msg smith-msg-assistant" style={{ maxWidth: "100%" }}>
        <div className="card" style={{ padding: "10px 14px" }}>
          <span style={{ color: "var(--text-mute)" }}>Action #{actionId} is no longer available.</span>
        </div>
      </div>
    );
  }
  return (
    <div className="smith-msg smith-msg-assistant" style={{ maxWidth: "100%" }}>
      <ActionCard action={action.data} />
    </div>
  );
}

// ResolutionBanner (Sprint S3-Web §2.4.1): when an action's post-verify
// succeeds and the investigation closes, smith posts a summary message to
// the linked conversation (finishResolution in investigations.go). This
// renders that summary as a visually distinct banner rather than a regular
// chat bubble, so the operator can see at a glance that the issue is
// resolved. The summary text patterns are stable (controlled by
// finishResolution): "fixed — action #N completed…" or "action #N executed
// but M check(s) still failing…".
const RESOLUTION_RE = /^(fixed — action #\d+|action #\d+ executed but)/;

function isResolutionBanner(msg: SmithMessage): boolean {
  return msg.kind === "smith_deterministic" && msg.content !== "" && RESOLUTION_RE.test(msg.content);
}

function ResolutionBanner({ msg }: { msg: SmithMessage }) {
  const isFixed = msg.content.startsWith("fixed — action #");
  const color = isFixed ? "var(--ok)" : "var(--warn)";
  return (
    <div
      className="smith-msg smith-msg-resolution"
      style={{
        borderLeft: `3px solid ${color}`,
        background: `color-mix(in srgb, ${color} 8%, var(--panel))`,
        padding: "8px 12px",
        borderRadius: 6,
        marginBottom: 8,
      }}
    >
      <Markdown text={msg.content} />
    </div>
  );
}

function MessageBubble({
  msg,
  streamingText,
  toolActivity,
  turnStatus,
  onDigDeeper,
}: {
  msg: SmithMessage;
  streamingText: string;
  toolActivity: ReturnType<typeof useSmithToolActivity>;
  turnStatus: string;
  onDigDeeper?: () => void;
}) {
  if (msg.kind === "user") {
    return (
      <div className="smith-msg smith-msg-user">
        <div className="smith-bubble">{msg.content}</div>
      </div>
    );
  }
  if (msg.kind === "notice") {
    return <div className="smith-msg smith-msg-notice">{msg.content}</div>;
  }
  // Sprint S3-Web's resolution banner (§2.4.1): when an action's post-verify
  // succeeds and the investigation closes, smith posts a summary to the
  // conversation. Render it as a visually distinct banner.
  if (isResolutionBanner(msg)) {
    return <ResolutionBanner msg={msg} />;
  }
  if (msg.kind === "action" || msg.kind === "runbook") {
    // Sprint S3-Go/S3-Web (§2.4.2): action-kind messages carry
    // {"action_id": N} evidence; NOT a serialized action (it would go stale
    // through pending→executing→done). The FE resolves live state via
    // GET /actions/{id} + smith:action_update SSE (LiveActionCard above).
    // Runbook-kind messages still carry {"steps": [...]} evidence (a
    // standalone runbook proposal, not tied to an action row).
    let parsed: unknown = null;
    try {
      parsed = msg.evidence ? JSON.parse(msg.evidence) : null;
    } catch {
      parsed = null;
    }
    if (msg.kind === "action" && parsed && typeof parsed === "object" && "action_id" in (parsed as object)) {
      return <LiveActionCard actionId={(parsed as { action_id: number }).action_id} />;
    }
    if (msg.kind === "runbook" && parsed && typeof parsed === "object" && "steps" in (parsed as object)) {
      return (
        <div className="smith-msg smith-msg-assistant" style={{ maxWidth: "100%" }}>
          <div className="card">
            <RunbookCard steps={(parsed as { steps: SmithRunbookStep[] }).steps} />
          </div>
        </div>
      );
    }
  }
  if (msg.kind === "tool_call") {
    // P7: one tool-loop round, persisted via appendToolCallMessage
    // (tool_loop.go). Falls through to a plain bubble on a shape mismatch,
    // same defensive posture as the action/runbook branch above.
    let evidence: SmithToolCallEvidence | null = null;
    try {
      evidence = msg.evidence ? (JSON.parse(msg.evidence) as SmithToolCallEvidence) : null;
    } catch {
      evidence = null;
    }
    if (evidence && Array.isArray(evidence.calls)) {
      return (
        <div className="smith-msg smith-msg-assistant" style={{ maxWidth: "100%" }}>
          <ToolCallCard evidence={evidence} />
        </div>
      );
    }
  }

  // Fast-path evidence (S2-Go, reasoning.go:871): a smith_deterministic
  // message with empty content + a JSON array of {label, value} evidence
  // rows. This is the expandable evidence detail that follows a fast answer;
  // render it as a continuation of the preceding answer (no bubble, no
  // meta). Falls through to the default bubble if the evidence doesn't
  // parse as the expected array shape (defensive, same posture as the
  // action/runbook parsing above).
  if (msg.kind === "smith_deterministic" && msg.content === "") {
    const rows = parseEvidence(msg.evidence);
    if (rows) {
      return (
        <div className="smith-msg smith-msg-assistant smith-msg-evidence">
          <EvidenceBlock evidence={rows} />
        </div>
      );
    }
  }

  const hasContent = msg.content !== "";
  const liveText = hasContent ? msg.content : streamingText;
  const pending = !hasContent && streamingText === "";
  const { answer, hasDigDeeper } = stripDigDeeper(liveText);

  return (
    <div className="smith-msg smith-msg-assistant">
      <div className="smith-msg-meta">
        <span>smith</span>
        {msg.model && <span>· {msg.model}</span>}
      </div>
      <div className="smith-bubble">
        {pending ? (
          <>
            <ToolActivityList events={toolActivity} />
            <span style={{ color: "var(--text-mute)" }}>
              {turnStatus || (toolActivity.length > 0 ? "…" : "thinking…")}
            </span>
          </>
        ) : (
          <>
            <Markdown text={answer} />
            {!hasContent && <span className="smith-streaming-cursor" />}
          </>
        )}
        {msg.error && <div className="error-note" style={{ marginTop: 6 }}>{msg.error}</div>}
      </div>
      {hasDigDeeper && onDigDeeper && <DigDeeperChip onClick={onDigDeeper} />}
      <SourcesList sources={msg.sources} />
    </div>
  );
}

// SweepControls (Quick sweep / Deep sweep / Custom picker): moved into the
// chat card from its own Diagnostics section, 2026-08-27, styled as the
// same .smith-quick-btn chips as the quick-question suggestions above the
// chat input rather than a separate bordered card of .tab pills.
function SweepControls() {
  const checks = useSmithChecks();
  const checksRun = useSmithChecksRun();
  const [showPicker, setShowPicker] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [result, setResult] = useState<{ count: number; worst: string } | null>(null);

  function run(scope: string) {
    setResult(null);
    checksRun.mutate(
      { scope },
      {
        onSuccess: (r) => setResult({ count: r.count, worst: r.worst }),
        onError: () => setResult(null),
      },
    );
  }

  function runCustom() {
    setShowPicker(false);
    setResult(null);
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    checksRun.mutate(
      { checkIds: ids },
      {
        onSuccess: (r) => setResult({ count: r.count, worst: r.worst }),
        onError: () => setResult(null),
      },
    );
  }

  const grouped = useMemo(() => {
    const m = new Map<string, SmithCheckMeta[]>();
    for (const c of checks.data?.checks ?? []) {
      if (!m.has(c.category)) m.set(c.category, []);
      m.get(c.category)!.push(c);
    }
    return m;
  }, [checks.data]);

  return (
    <div style={{ marginTop: 6 }}>
      <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
        <button className="smith-quick-btn" onClick={() => run("quick")} disabled={checksRun.isPending}>
          {checksRun.isPending && !showPicker ? "…" : "Quick sweep"}
        </button>
        <button className="smith-quick-btn" onClick={() => run("deep")} disabled={checksRun.isPending}>
          Deep sweep
        </button>
        <button className="smith-quick-btn" onClick={() => setShowPicker(!showPicker)} disabled={checksRun.isPending}>
          Custom…
        </button>
      </div>

      {checksRun.isError && (
        <div className="error-note" style={{ marginTop: 6 }}>
          {apiErrorMessage(checksRun.error)}
        </div>
      )}

      {result && <SweepResult count={result.count} worst={result.worst} />}

      {showPicker && (
        <div style={{ marginTop: 10, borderTop: "1px solid var(--border)", paddingTop: 10 }}>
          {checks.isLoading ? (
            <div className="empty-note">Loading check catalog…</div>
          ) : (
            <>
              {Array.from(grouped.entries()).map(([cat, items]) => (
                <div key={cat} style={{ marginBottom: 8 }}>
                  <div style={{ fontSize: 11, color: "var(--text-mute)", textTransform: "uppercase", letterSpacing: "0.05em", marginBottom: 4 }}>
                    {cat}
                  </div>
                  {items.map((c) => (
                    <label
                      key={c.id}
                      style={{ display: "inline-flex", alignItems: "center", gap: 4, marginRight: 12, fontSize: 12, color: "var(--text-dim)", cursor: "pointer" }}
                    >
                      <input
                        type="checkbox"
                        checked={selected.has(c.id)}
                        onChange={() => {
                          const next = new Set(selected);
                          if (next.has(c.id)) next.delete(c.id);
                          else next.add(c.id);
                          setSelected(next);
                        }}
                        style={{ cursor: "pointer" }}
                      />
                      {c.name}
                      {c.fast && <span className="chip" style={{ fontSize: 9, padding: "0 4px" }}>fast</span>}
                    </label>
                  ))}
                </div>
              ))}
              <div style={{ display: "flex", gap: 6, marginTop: 6 }}>
                <button
                  className="smith-quick-btn"
                  onClick={runCustom}
                  disabled={checksRun.isPending || selected.size === 0}
                >
                  Run {selected.size} check{selected.size === 1 ? "" : "s"}
                </button>
                <button className="smith-quick-btn" onClick={() => { setSelected(new Set()); setShowPicker(false); }}>
                  Cancel
                </button>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}

// SuggestionsPanel: pending smith actions ("Needs your approval" and
// "Suggestions" merged into one list, 2026-08-27), now living inside the
// chat card itself (below the chat box) instead of its own separate card
// on Diagnostics further down the page; it's part of the same
// conversation loop as the chat, not a diagnostics artifact. Collapsible,
// default open: these can need a real approve/reject decision, so they
// shouldn't start hidden the way the more occasional
// ProcedureRuns/BlockedWork sections further down do.
function SuggestionsPanel() {
  const actions = useSmithActions("pending");
  const [expanded, setExpanded] = useState(true);
  if (actions.isLoading || actions.isError) return null;
  const list = actions.data?.actions ?? [];
  if (list.length === 0) return null;
  return (
    <div className="smith-suggestions" style={{ marginTop: 10, paddingTop: 10, borderTop: "1px solid var(--border)" }}>
      <button
        className="tab"
        style={{ fontSize: 11, padding: 0, fontWeight: 600 }}
        onClick={() => setExpanded((v) => !v)}
      >
        {expanded ? "▾" : "▸"} Suggestions ({list.length})
      </button>
      {expanded && (
        <div style={{ marginTop: 8 }}>
          {list.map((a) => (
            <ActionCard key={a.id} action={a} />
          ))}
        </div>
      )}
    </div>
  );
}

export function AskSmith({
  sub,
  onSubChange,
}: {
  sub?: string;
  onSubChange?: (sub: string, opts?: { replace?: boolean }) => void;
}) {
  const conversations = useSmithConversations();
  const [selectedId, setSelectedId] = useState<number | null>(() => parseConvId(sub));
  const [showList, setShowList] = useState(false);
  const [text, setText] = useState("");
  const [pendingMessageId, setPendingMessageId] = useState<number | null>(null);
  const [sendError, setSendError] = useState<string | null>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);

  function focusInput() {
    inputRef.current?.focus();
  }

  useEffect(() => {
    const id = parseConvId(sub);
    if (id !== null) setSelectedId(id);
  }, [sub]);

  const conv = useSmithConversation(selectedId);
  const status = useSmithStatus();
  const chat = useSmithChat();
  const del = useSmithConversationDelete();
  const pendingText = useSmithStreamingText(pendingMessageId);
  const pendingToolActivity = useSmithToolActivity(pendingMessageId);
  const pendingTurnStatus = useSmithTurnStatus(pendingMessageId);
  const { pending, clear } = useSmithPendingAsk();
  // Guards the pending-ask effect against double-consumption (React
  // StrictMode re-runs mount effects in dev; without this the same seeded
  // context turn would be posted twice).
  const pendingAskHandledRef = useRef<string | null>(null);

  useEffect(() => {
    transcriptRef.current?.scrollTo({ top: transcriptRef.current.scrollHeight, behavior: pendingText ? "auto" : "smooth" });
  }, [conv.data?.messages.length, pendingText]);

  // Sprint S3-Web (§2.3, R5): when an "Ask smith" affordance on an error row
  // routes here with attached context, fire a context-seeded turn: text=""
  // + context[]; the server composes the seed message itself
  // (composeContextSeedMessage), so no FE string-formatting. Cleared
  // immediately (before the send) so a rapid re-click or a remount can't
  // double-post.
  useEffect(() => {
    if (!pending || pending.context.length === 0) return;
    const key = JSON.stringify(pending.context);
    if (pendingAskHandledRef.current === key) return;
    pendingAskHandledRef.current = key;
    clear();
    setSendError(null);
    chat.mutate(
      {
        conversation_id: selectedId ?? undefined,
        text: "",
        context: pending.context,
      },
      {
        onSuccess: (resp) => {
          setText("");
          setPendingMessageId(resp.message_id);
          if (resp.conversation_id !== selectedId) selectConversation(resp.conversation_id);
        },
        onError: (e) => setSendError(apiErrorMessage(e)),
      }
    );
  }, [pending]);

  // Once the placeholder row's persisted content lands (smith:message_done
  // reconciled it), stop tracking it as streaming; the row is
  // authoritative from here on, whatever the SSE stream did or didn't drop.
  useEffect(() => {
    if (pendingMessageId == null) return;
    const row = conv.data?.messages.find((m) => m.id === pendingMessageId);
    if (row && row.content !== "") setPendingMessageId(null);
  }, [conv.data, pendingMessageId]);

  const hammerClass = useMemo(() => {
    if (pendingMessageId != null) return "smith-hammer--working";
    const msgs = conv.data?.messages;
    if (msgs && msgs.length > 0) {
      const last = msgs[msgs.length - 1];
      if (last.kind !== "user" && last.kind !== "notice" && last.content.trim().endsWith("?"))
        return "smith-hammer--waiting";
    }
    return "smith-hammer--done";
  }, [pendingMessageId, conv.data]);

  function selectConversation(id: number | null) {
    setSelectedId(id);
    setShowList(false);
    onSubChange?.(id ? `smith/conv/${id}` : "smith", { replace: true });
  }

  function send(overrideText?: string) {
    const trimmed = (overrideText ?? text).trim();
    if (!trimmed || chat.isPending) return;
    setSendError(null);
    chat.mutate(
      { conversation_id: selectedId ?? undefined, text: trimmed },
      {
        onSuccess: (resp) => {
          setText("");
          setPendingMessageId(resp.message_id);
          if (resp.conversation_id !== selectedId) selectConversation(resp.conversation_id);
        },
        onError: (e) => setSendError(apiErrorMessage(e)),
      }
    );
  }

  function handleKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  }

  return (
    <>
      <div className="card" style={{ marginBottom: 12 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 10, flexWrap: "wrap" }}>
          <HammerIcon className={`smith-hammer ${hammerClass}`} />
          <span style={{ fontWeight: 700, fontSize: 13, letterSpacing: "0.02em" }}>SMITH</span>
          <button className="tab" onClick={() => setShowList((v) => !v)}>
            {conv.data ? conv.data.title || `Conversation #${conv.data.id}` : "Conversations"} ▾
          </button>
          <button className="tab" onClick={() => selectConversation(null)}>
            + New conversation
          </button>
          {selectedId != null && (
            <ConfirmButton
              onConfirm={() => del.mutate(selectedId, { onSuccess: () => selectConversation(null) })}
              pending={del.isPending}
              label="Delete"
              confirmLabel="Delete?"
              warning="This conversation and its transcript will be deleted."
            />
          )}
          {status.data && (
            <div style={{ marginLeft: "auto" }}>
              <SmithIndicators status={status.data} />
            </div>
          )}
        </div>

        {showList && (
          <div className="smith-conv-list" style={{ marginBottom: 10 }}>
            {conversations.isLoading ? (
              <div className="empty-note">Loading…</div>
            ) : conversations.data && conversations.data.conversations.length > 0 ? (
              conversations.data.conversations.map((c) => (
                <div
                  key={c.id}
                  className={`smith-conv-row ${c.id === selectedId ? "active" : ""}`}
                  onClick={() => selectConversation(c.id)}
                >
                  <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                    {c.title || `Conversation #${c.id}`}
                  </span>
                </div>
              ))
            ) : (
              <div className="empty-note">No conversations yet.</div>
            )}
          </div>
        )}

        <div ref={transcriptRef} className="smith-transcript" style={{ maxHeight: 480, overflowY: "auto", paddingRight: 4 }}>
          {selectedId == null ? null : conv.isLoading ? (
            <div className="empty-note">Loading conversation…</div>
          ) : conv.isError ? (
            <div className="error-note">{apiErrorMessage(conv.error)}</div>
          ) : conv.data && conv.data.messages.length === 0 ? (
            <div className="empty-note">No messages yet. Say something below.</div>
          ) : (
            conv.data?.messages.map((m) => (
              <MessageBubble
                key={m.id}
                msg={m}
                streamingText={m.id === pendingMessageId ? pendingText : ""}
                toolActivity={m.id === pendingMessageId ? pendingToolActivity : []}
                turnStatus={m.id === pendingMessageId ? pendingTurnStatus : ""}
                onDigDeeper={focusInput}
              />
            ))
          )}
        </div>

        <div className="smith-suggestions" style={{ marginBottom: 8 }}>
          <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
            {QUICK_QUESTIONS.map((q) => (
              <button
                key={q}
                className="smith-quick-btn"
                disabled={chat.isPending}
                onClick={() => send(q)}
              >
                {q}
              </button>
            ))}
          </div>
          <SweepControls />
        </div>

        <div className="smith-chat-input">
          <textarea
            ref={inputRef}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Ask the smith… (Enter to send, Shift+Enter for a new line)"
            rows={3}
          />
          <button className="btn primary" onClick={() => send()} disabled={chat.isPending || !text.trim()}>
            {chat.isPending ? "…" : "Ask SMITH"}
          </button>
        </div>
        {sendError && <div className="error-note" style={{ marginTop: 6 }}>{sendError}</div>}

        <SuggestionsPanel />
      </div>
    </>
  );
}
