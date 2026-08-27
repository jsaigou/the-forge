package cli

// Wire shapes for the dashboard API endpoints the CLI/TUI consume.
// Field names mirror go/internal/httpapi/shapes.go — keep in lockstep.

// Status is GET /api/v1/status (buildStatusResponse).
type Status struct {
	Mode          string                 `json:"mode"`
	Description   string                 `json:"description"`
	Services      map[string]string      `json:"services"`
	Ports         map[string]bool        `json:"ports"`
	Slots         map[string]*string     `json:"slots"`
	SlotLabels    map[string]string      `json:"slot_labels"`
	ModesAvail    map[string]ModeAvail   `json:"modes_available"`
	ServiceModes  map[string]SvcModeInfo `json:"service_modes"`
	Hostname      string                 `json:"hostname"`
	Version       string                 `json:"version"`
	TTSActive     bool                   `json:"tts_active"`
	Switch        SwitchState            `json:"switch"`
	SlotLoading   map[string]SlotState   `json:"slot_loading"`
	SlotUnloading map[string]SlotState   `json:"slot_unloading"`
	Alerts        []Alert                `json:"alerts,omitempty"`
	SlotActivity  map[string]bool        `json:"slot_activity"`
}

// ModeAvail is one entry of status.modes_available.
type ModeAvail struct {
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Family      string   `json:"family"`
	Tags        []string `json:"tags"`
	Color       string   `json:"color"`
	Context     int      `json:"context"`
	SlotCapable bool     `json:"slot_capable"`
}

// SvcModeInfo is one entry of status.service_modes.
type SvcModeInfo struct {
	Label  string  `json:"label"`
	Icon   string  `json:"icon"`
	Unit   *string `json:"unit"`
	Active bool    `json:"active"`
}

// SwitchState is status.switch.
type SwitchState struct {
	InProgress bool     `json:"in_progress"`
	Target     *string  `json:"target"`
	StartedAt  *float64 `json:"started_at"`
}

// SlotState is a slot load/unload progress entry.
type SlotState struct {
	InProgress bool     `json:"in_progress"`
	Target     *string  `json:"target"`
	StartedAt  *float64 `json:"started_at"`
}

// Alert is a status alert entry (severity/message at minimum).
type Alert struct {
	Severity string `json:"severity"`
	CheckID  string `json:"check_id,omitempty"`
	Message  string `json:"message"`
}

// Metrics is GET /api/v1/metrics.
type Metrics struct {
	Mode          string        `json:"mode"`
	Memory        MetricsMemory `json:"memory"`
	CPU           MetricsCPU    `json:"cpu"`
	GPUUsePct     *float64      `json:"gpu_use_pct"`
	GTTUsedBytes  *int64        `json:"gtt_used_bytes"`
	GTTTotalBytes *int64        `json:"gtt_total_bytes"`
	TempCelsius   *float64      `json:"temp_celsius"`
	UptimeSeconds *int64        `json:"uptime_seconds"`
}

// MetricsMemory is metrics.memory.
type MetricsMemory struct {
	UsedBytes  *int64   `json:"used_bytes,omitempty"`
	FreeBytes  *int64   `json:"free_bytes,omitempty"`
	TotalBytes *int64   `json:"total_bytes,omitempty"`
	PctUsed    *float64 `json:"pct_used,omitempty"`
}

// MetricsCPU is metrics.cpu.
type MetricsCPU struct {
	Percent *float64 `json:"percent,omitempty"`
}

// SchedulerStatus is GET /api/v1/scheduler/status.
type SchedulerStatus struct {
	Slots           map[string]*string  `json:"slots"`
	SlotLabels      map[string]string   `json:"slot_labels"`
	IdleSeconds     map[string]*float64 `json:"idle_seconds"`
	SlotMemoryBytes map[string]int64    `json:"slot_memory_bytes"`
	UnitMemoryBytes map[string]int64    `json:"unit_memory_bytes"`
	MemoryBudget    SchedBudget         `json:"memory_budget"`
	Queue           []QueueTicket       `json:"queue"`
}

// SchedBudget is the memory budget triple in bytes.
type SchedBudget struct {
	TotalBytes int64 `json:"total_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
}

// QueueTicket is one scheduler queue entry.
type QueueTicket struct {
	TicketID    string  `json:"ticket_id"`
	Model       string  `json:"model"`
	RequestedBy string  `json:"requested_by"`
	TargetSlot  *string `json:"target_slot"`
	Status      string  `json:"status"`
	SmallJob    bool    `json:"small_job,omitempty"`
	EnqueuedAt  float64 `json:"enqueued_at"`
}

// ConfigCard is one entry of GET /api/v1/configs/cards (registry.ConfigCard).
type ConfigCard struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ModelID   string `json:"model_id"`
	NCtx      int    `json:"n_ctx"`
	Status    string `json:"status"`
	IsDefault bool   `json:"is_default"`
}

// LoadRequest is POST /api/v1/load {mode,slot}.
type LoadRequest struct {
	Mode string `json:"mode"`
	Slot string `json:"slot"`
}

// UnloadRequest is POST /api/v1/unload {slot}.
type UnloadRequest struct {
	Slot string `json:"slot"`
}

// LifecycleResult is the common {success,message,n_ctx} reply.
type LifecycleResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	NCtx    int    `json:"n_ctx,omitempty"`
}

// InfraServices is GET /api/v1/infra-services.
type InfraServices struct {
	Services []InfraService `json:"services"`
}

// InfraService is one always-on / service-mode row.
type InfraService struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Unit   string `json:"unit,omitempty"`
	Active bool   `json:"active"`
	Port   int    `json:"port,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// NotificationsResponse is GET /api/v1/notifications.
type NotificationsResponse struct {
	Notifications []Notification `json:"notifications"`
}

// Notification is one alert/notice row.
type Notification struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	Subject     string `json:"subject,omitempty"`
	Message     string `json:"message"`
	FirstSeen   int64  `json:"first_seen"`
	LastSeen    int64  `json:"last_seen"`
	Occurrences int    `json:"occurrences"`
}

// CompressorSummary is GET /api/v1/compressor/summary.
type CompressorSummary struct {
	Window  string                   `json:"window"`
	Proxies []CompressorSummaryProxy `json:"proxies"`
}

// CompressorSummaryProxy is per-proxy savings/latency stats.
type CompressorSummaryProxy struct {
	Proxy           string   `json:"proxy"`
	Kind            string   `json:"kind"`
	TokensIn        int64    `json:"tokens_in"`
	TokensOut       int64    `json:"tokens_out"`
	TokensSaved     int64    `json:"tokens_saved"`
	Requests        int64    `json:"requests"`
	RequestsCached  int64    `json:"requests_cached"`
	CacheHitRatePct *float64 `json:"cache_hit_rate_pct"`
	LatencyMeanMs   *float64 `json:"latency_mean_ms"`
	OverheadMeanMs  *float64 `json:"overhead_mean_ms"`
}

// ProvidersResponse is GET /api/v1/providers.
type ProvidersResponse struct {
	Providers []Provider `json:"providers"`
}

// Provider is one remote offering credential row (providerJSON).
type Provider struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	APIKeyMasked string `json:"api_key_masked"`
}

// SmithMessage mirrors smith.Message for conversation rendering.
type SmithMessage struct {
	ID        int64   `json:"id"`
	Kind      string  `json:"kind"`
	Content   string  `json:"content"`
	Tier      *string `json:"tier"`
	Error     *string `json:"error"`
	CreatedAt int64   `json:"created_at"`
}

// SmithConversationDetail is GET /api/v1/smith/conversations/{id}.
type SmithConversationDetail struct {
	Messages []SmithMessage `json:"messages"`
}

// KeysResponse is GET /api/v1/keys.
type KeysResponse struct {
	Keys []APIKey `json:"keys"`
}

// APIKey is one minted dashboard key (secret never returned). Field names
// were found out of lockstep with the real server shape (KeyID tagged
// "key_id" against the server's "keyid") while wiring #34/#36 — the TUI's
// Keys-page revoke action always sent an empty keyid as a result; fixed
// alongside the new bound_ip/expires_at fields.
type APIKey struct {
	KeyID     string   `json:"keyid"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Role      string   `json:"role,omitempty"`
	BoundIP   string   `json:"bound_ip,omitempty"`
	CreatedAt float64  `json:"created_at,omitempty"`
	LastUsed  *float64 `json:"last_used_at,omitempty"`
	ExpiresAt *float64 `json:"expires_at,omitempty"`
}

// KeyCreateRequest is POST /api/v1/keys.
type KeyCreateRequest struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	Role            string `json:"role,omitempty"`
	BindToRequester bool   `json:"bind_to_requester,omitempty"`
	TTLSeconds      int64  `json:"ttl_seconds,omitempty"`
}

// KeyCreateResponse returns the plaintext secret exactly once. Mirrors the
// server's real nested shape (token + key, not a top-level key_id).
type KeyCreateResponse struct {
	Token string `json:"token"`
	Key   APIKey `json:"key"`
}

// SmithChatRequest is POST /api/v1/smith/chat.
type SmithChatRequest struct {
	ConversationID int64  `json:"conversation_id"`
	Text           string `json:"text"`
	Escalate       bool   `json:"escalate"`
	Web            bool   `json:"web"`
}

// SmithChatReply is the immediate 202 body; the answer streams over SSE
// as smith:token deltas on the conversation.
type SmithChatReply struct {
	ConversationID int64 `json:"conversation_id"`
	MessageID      int64 `json:"message_id"`
}
