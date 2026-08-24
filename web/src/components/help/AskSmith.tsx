import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { apiErrorMessage } from "../../lib/api";
import {
  useSmithAction,
  useSmithChat,
  useSmithConversation,
  useSmithConversationDelete,
  useSmithConversations,
  useSmithPendingAsk,
  useSmithStatus,
  useSmithStreamingText,
  useSmithToolActivity,
} from "../../lib/queries";
import type { SmithMessage, SmithRunbookStep, SmithToolCallEvidence } from "../../lib/types";
import { ConfirmButton } from "../ConfirmButton";
import { HammerIcon } from "../icons/HammerIcon";
import { ActionCard } from "../smith/ActionCard";
import { DigDeeperChip, stripDigDeeper } from "../smith/DigDeeperChip";
import { EvidenceBlock, parseEvidence } from "../smith/EvidenceBlock";
import { Markdown } from "../smith/Markdown";
import { RunbookCard } from "../smith/RunbookCard";
import { SelfContextChip } from "../smith/SelfContextChip";
import { SourcesList } from "../smith/SourcesList";
import { ToolActivityList, ToolCallCard } from "../smith/ToolCallCard";

// Ask the smith — the P3 chat sub-tab (docs/v5-smith.md §6). Streaming rides
// the existing single app-wide EventSource (lib/sse.ts): smith:token
// deltas land in a client-only cache slot (qk.smith.streaming, read via
// useSmithStreamingText) keyed by the placeholder assistant message ID
// POST /chat returns; smith:message_done invalidates the conversation so
// the persisted row (real content, model, tier) takes over from the
// streamed text — that handoff is what MessageBubble's `hasContent` check
// below implements.

function parseConvId(sub?: string): number | null {
  const m = sub?.match(/conv\/(\d+)/);
  return m ? parseInt(m[1], 10) : null;
}

// LiveActionCard — Sprint S3-Web (§2.4.2): action-kind messages carry
// {"action_id": N} evidence (NOT a serialized action — it would go stale
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

// ResolutionBanner — Sprint S3-Web (§2.4.1): when an action's post-verify
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
  onDigDeeper,
}: {
  msg: SmithMessage;
  streamingText: string;
  toolActivity: ReturnType<typeof useSmithToolActivity>;
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
  // Sprint S3-Web — resolution banner (§2.4.1): when an action's post-verify
  // succeeds and the investigation closes, smith posts a summary to the
  // conversation. Render it as a visually distinct banner.
  if (isResolutionBanner(msg)) {
    return <ResolutionBanner msg={msg} />;
  }
  if (msg.kind === "action" || msg.kind === "runbook") {
    // Sprint S3-Go/S3-Web (§2.4.2): action-kind messages carry
    // {"action_id": N} evidence — NOT a serialized action (it would go stale
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
    // P7 — one tool-loop round, persisted via appendToolCallMessage
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
  // rows. This is the expandable evidence detail that follows a fast answer
  // — render it as a continuation of the preceding answer (no bubble, no
  // meta). Falls through to the default bubble if the evidence doesn't
  // parse as the expected array shape (defensive — same posture as the
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
              {toolActivity.length > 0 ? "…" : "thinking…"}
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

export function AskSmith({
  sub,
  onSubChange,
}: {
  sub?: string;
  onSubChange?: (sub: string, opts?: { replace?: boolean }) => void;
}) {
  const status = useSmithStatus();
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
  const chat = useSmithChat();
  const del = useSmithConversationDelete();
  const pendingText = useSmithStreamingText(pendingMessageId);
  const pendingToolActivity = useSmithToolActivity(pendingMessageId);
  const { pending, clear } = useSmithPendingAsk();
  // Guards the pending-ask effect against double-consumption (React
  // StrictMode re-runs mount effects in dev; without this the same seeded
  // context turn would be posted twice).
  const pendingAskHandledRef = useRef<string | null>(null);

  useEffect(() => {
    transcriptRef.current?.scrollTo({ top: transcriptRef.current.scrollHeight, behavior: pendingText ? "auto" : "smooth" });
  }, [conv.data?.messages.length, pendingText]);

  // Sprint S3-Web (§2.3, R5): when an "Ask smith" affordance on an error row
  // routes here with attached context, fire a context-seeded turn — text=""
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
  // reconciled it), stop tracking it as streaming — the row is
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

  function send() {
    const trimmed = text.trim();
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
        {status.isLoading ? (
          <div className="empty-note">Loading smith status…</div>
        ) : status.isError ? (
          <div className="error-note">Unable to reach smith: {apiErrorMessage(status.error)}</div>
        ) : status.data ? (
          <SelfContextChip status={status.data} />
        ) : null}
      </div>

      <div className="card" style={{ marginBottom: 12 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 10, flexWrap: "wrap" }}>
          <HammerIcon className={`smith-hammer ${hammerClass}`} />
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
          {selectedId == null ? (
            <div className="smith-quick-questions">
              <div className="empty-note" style={{ marginBottom: 10 }}>
                Ask the smith about the box — memory, GPU, network, services, or anything you notice.
              </div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
                {[
                  "Is ComfyUI healthy?",
                  "How much memory is free?",
                  "What services are degraded?",
                  "Is A0 working?",
                  "What's in the backlog?",
                  "Are there pending investigations?",
                ].map((q) => (
                  <button key={q} className="tab" style={{ fontSize: 11 }} onClick={() => setText(q)}>
                    {q}
                  </button>
                ))}
              </div>
            </div>
          ) : conv.isLoading ? (
            <div className="empty-note">Loading conversation…</div>
          ) : conv.isError ? (
            <div className="error-note">{apiErrorMessage(conv.error)}</div>
          ) : conv.data && conv.data.messages.length === 0 ? (
            <div className="empty-note">No messages yet — say something below.</div>
          ) : (
            conv.data?.messages.map((m) => (
              <MessageBubble
                key={m.id}
                msg={m}
                streamingText={m.id === pendingMessageId ? pendingText : ""}
                toolActivity={m.id === pendingMessageId ? pendingToolActivity : []}
                onDigDeeper={focusInput}
              />
            ))
          )}
        </div>

        <div className="smith-chat-input">
          <textarea
            ref={inputRef}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Ask the smith… (Enter to send, Shift+Enter for a new line)"
            rows={2}
          />
          <button className="btn primary" onClick={send} disabled={chat.isPending || !text.trim()}>
            {chat.isPending ? "…" : "Send"}
          </button>
        </div>
        {sendError && <div className="error-note" style={{ marginTop: 6 }}>{sendError}</div>}
      </div>
    </>
  );
}
