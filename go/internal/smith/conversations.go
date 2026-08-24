// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Message kinds (smith_messages.kind, migration 0033). user/action/runbook
// mirror what the FE already renders via ActionCard/RunbookCard for the
// action/runbook rows; smith_deterministic/smith_reasoning distinguish which
// tier produced an answer (the FE's tier chip reads this per-message, not
// just off the conversation, since a conversation can span a mid-stream
// degrade); notice is a system-authored note (Tier 2 failure, degrade, or
// handoff commentary) with no evidence/model attached.
const (
	MsgKindUser          = "user"
	MsgKindDeterministic = "smith_deterministic"
	MsgKindReasoning     = "smith_reasoning"
	MsgKindAction        = "action"
	MsgKindRunbook       = "runbook"
	MsgKindNotice        = "notice"
	// MsgKindToolCall (P7) is one tool-loop round's activity — reuses the
	// evidence column (unclaimed for this kind; sources belongs to the
	// answer row, not a tool round) rather than adding a new column.
	// smith_messages.kind has no CHECK constraint (migration 0033), so
	// this costs no migration. tool_loop.go's appendToolCallMessage is the
	// only writer; AskSmith.tsx's MessageBubble is the only reader.
	MsgKindToolCall = "tool_call"
)

// Conversation is one smith_conversations row (docs/v5-smith.md §4.4).
type Conversation struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Tier      string `json:"tier"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Message is one smith_messages row. Model/Tier/Error/TokenCount are the
// 0035 columns — nil for kinds that don't apply (a `user` row never carries
// a model; a `notice` row from a clean run never carries an error). Sources
// (0037, smith P5) is a separate carrier from Evidence — Evidence is already
// claimed by the action/runbook message kinds (the FE parses it as an
// ActionCard/RunbookCard shape by branching on which keys are present), and
// finalizeMessage/failMessage never touch it, so a mid-turn web-research
// result needs its own column to survive a failed or degraded turn.
type Message struct {
	ID             int64           `json:"id"`
	ConversationID int64           `json:"conversation_id"`
	Kind           string          `json:"kind"`
	Content        string          `json:"content"`
	Evidence       *string         `json:"evidence"` // raw JSON text, nullable
	Model          *string         `json:"model"`
	Tier           *string         `json:"tier"`
	Error          *string         `json:"error"`
	TokenCount     *int            `json:"token_count"`
	CreatedAt      int64           `json:"created_at"`
	Sources        []MessageSource `json:"sources"` // never nil — [] when none
}

// MessageSource is one citation smith read while answering (P5 web
// research, docs/v5-smith.md §4.8) — the minimal record needed to render a
// collapsible sources list in the transcript.
type MessageSource struct {
	Provider  string `json:"provider"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	FetchedAt int64  `json:"fetched_at"` // unix seconds
	Cached    bool   `json:"cached"`
}

// CreateConversation inserts a new conversation and returns its ID. title
// may be empty (the FE falls back to "New conversation" / the first user
// message).
func (s *Smith) CreateConversation(ctx context.Context, title string) (int64, error) {
	if s.d.Store == nil {
		return 0, ErrStoreUnwired
	}
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`INSERT INTO smith_conversations (title, tier, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`,
		title, TierDeterministic, now, now)
	if err != nil {
		return 0, fmt.Errorf("smith: create conversation: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ListConversations returns conversations, most-recently-updated first.
func (s *Smith) ListConversations(ctx context.Context) ([]Conversation, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id, title, tier, created_at, updated_at
		 FROM smith_conversations ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("smith: list conversations: %w", err)
	}
	defer rows.Close()

	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Tier, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("smith: scan conversation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversation returns a conversation and its messages, oldest first.
// Returns a wrapped sql.ErrNoRows if the conversation doesn't exist.
func (s *Smith) GetConversation(ctx context.Context, id int64) (*Conversation, []Message, error) {
	if s.d.Store == nil {
		return nil, nil, ErrStoreUnwired
	}
	var c Conversation
	err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT id, title, tier, created_at, updated_at FROM smith_conversations WHERE id = ?`, id).
		Scan(&c.ID, &c.Title, &c.Tier, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, nil, fmt.Errorf("smith: get conversation: %w", err)
	}

	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id, conversation_id, kind, content, evidence, model, tier, error, token_count, created_at, sources
		 FROM smith_messages WHERE conversation_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("smith: list messages: %w", err)
	}
	defer rows.Close()

	msgs := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return &c, msgs, nil
}

// DeleteConversation removes a conversation; smith_messages cascades via the
// 0033 FK (ON DELETE CASCADE). Idempotent — deleting a missing ID is not an
// error (matches the reject/cancel idempotency convention elsewhere here).
func (s *Smith) DeleteConversation(ctx context.Context, id int64) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`DELETE FROM smith_conversations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("smith: delete conversation: %w", err)
	}
	return nil
}

// messageRowScanner is satisfied by both *sql.Row and *sql.Rows.
type messageRowScanner interface {
	Scan(dest ...any) error
}

func scanMessage(row messageRowScanner) (Message, error) {
	var m Message
	var evidence, model, tier, errStr sql.NullString
	var tokenCount sql.NullInt64
	var sourcesRaw string
	if err := row.Scan(&m.ID, &m.ConversationID, &m.Kind, &m.Content,
		&evidence, &model, &tier, &errStr, &tokenCount, &m.CreatedAt, &sourcesRaw); err != nil {
		return Message{}, fmt.Errorf("smith: scan message: %w", err)
	}
	if evidence.Valid {
		m.Evidence = &evidence.String
	}
	if model.Valid {
		m.Model = &model.String
	}
	if tier.Valid {
		m.Tier = &tier.String
	}
	if errStr.Valid {
		m.Error = &errStr.String
	}
	if tokenCount.Valid {
		v := int(tokenCount.Int64)
		m.TokenCount = &v
	}
	m.Sources = decodeMessageSources(sourcesRaw)
	return m, nil
}

// decodeMessageSources tolerates an empty/null/malformed sources blob by
// falling back to an empty (never nil) slice — the same "never fake data,
// never panic" posture as decodeHealth/decodeCredits in internal/providers.
func decodeMessageSources(raw string) []MessageSource {
	if raw == "" {
		return []MessageSource{}
	}
	var out []MessageSource
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []MessageSource{}
	}
	return out
}

// appendMessage inserts a message row and bumps the parent conversation's
// updated_at in the same call (every conversation surface — the list view,
// SSE invalidation — sorts by updated_at, so an append that forgot this
// would silently vanish from a recency-sorted list). evidence/model/tier/
// errStr/tokenCount are nil-able optional columns; pass nil for any that
// don't apply to kind.
func (s *Smith) appendMessage(ctx context.Context, convID int64, kind, content string,
	evidence, model, tier, errStr *string, tokenCount *int) (int64, error) {
	if s.d.Store == nil {
		return 0, ErrStoreUnwired
	}
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`INSERT INTO smith_messages
			(conversation_id, kind, content, evidence, model, tier, error, token_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		convID, kind, content, evidence, model, tier, errStr, tokenCount, now)
	if err != nil {
		return 0, fmt.Errorf("smith: append message: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_conversations SET updated_at = ? WHERE id = ?`, now, convID); err != nil {
		return id, fmt.Errorf("smith: touch conversation: %w", err)
	}
	return id, nil
}

// setMessageSources persists what smith read during a turn's web research
// (P5, docs/v5-smith.md §4.8). Written independently of finalizeMessage/
// failMessage — neither touches this column — as soon as search+fetch
// resolve, mid-turn, so a turn that later fails or degrades still records
// what smith looked at ("never lose the transcript" extended to "never
// lose what you read").
func (s *Smith) setMessageSources(ctx context.Context, id int64, sources []MessageSource) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	if sources == nil {
		sources = []MessageSource{}
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return fmt.Errorf("smith: marshal sources: %w", err)
	}
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_messages SET sources = ? WHERE id = ?`, string(raw), id); err != nil {
		return fmt.Errorf("smith: set message sources: %w", err)
	}
	return nil
}

// AppendUserMessage records the operator's chat turn.
func (s *Smith) AppendUserMessage(ctx context.Context, convID int64, content string) (int64, error) {
	return s.appendMessage(ctx, convID, MsgKindUser, content, nil, nil, nil, nil, nil)
}

// finalizeMessage overwrites a placeholder row's content once a turn
// completes (streamed reasoning) or runs synchronously (deterministic).
// Distinct from appendMessage: the row already exists (created empty at
// turn-start so its ID is stable across the whole SSE stream) and only its
// terminal fields change.
func (s *Smith) finalizeMessage(ctx context.Context, id int64, content string, model, tier *string, tokenCount *int) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_messages SET content = ?, model = ?, tier = ?, token_count = ? WHERE id = ?`,
		content, model, tier, tokenCount, id); err != nil {
		return fmt.Errorf("smith: finalize message: %w", err)
	}
	return nil
}

// failMessage records a Tier 2 failure on a placeholder row — the message
// keeps whatever partial content had streamed in plus an error string, so
// the transcript never silently loses a partial answer (docs/v5-smith.md
// §4.3 "never lose the transcript").
func (s *Smith) failMessage(ctx context.Context, id int64, partialContent, errMsg string) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_messages SET content = ?, error = ? WHERE id = ?`,
		partialContent, errMsg, id); err != nil {
		return fmt.Errorf("smith: fail message: %w", err)
	}
	return nil
}

// AppendNotice records a system-authored note (degrade, handoff commentary).
func (s *Smith) AppendNotice(ctx context.Context, convID int64, content string) (int64, error) {
	return s.appendMessage(ctx, convID, MsgKindNotice, content, nil, nil, nil, nil, nil)
}

// setConversationTier updates smith_conversations.tier (called when a
// conversation's active tier changes — first successful reasoning turn, or
// a degrade back to deterministic).
func (s *Smith) setConversationTier(ctx context.Context, convID int64, tier string) error {
	if s.d.Store == nil {
		return ErrStoreUnwired
	}
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_conversations SET tier = ?, updated_at = ? WHERE id = ?`,
		tier, s.d.Now().Unix(), convID); err != nil {
		return fmt.Errorf("smith: set conversation tier: %w", err)
	}
	return nil
}
