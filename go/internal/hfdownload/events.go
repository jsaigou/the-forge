// SPDX-License-Identifier: Apache-2.0

package hfdownload

// events.go — Contract 1 amendment (HF model-acquisition track): four new
// namespaced SSE events, following the profile:* precedent
// (internal/profile/profile.go) rather than the legacy download_*
// underscore names bus.go's doc comment already reserves from V4 history —
// nothing in the v0.5 Go rewrite ever implemented those, and every other
// Sprint-era addition (profile:*, slot:activity, smith:*) uses the colon
// form, so this keeps the convention consistent rather than reviving an
// unused legacy name. internal/httpapi/httpapi_test.go's
// TestSSEEmitsUnderscoreEventNames allowlist must include these.
const (
	// EventProgress fires on every throttled progress sample:
	// {job_id, state, bytes_done, bytes_total, bytes_per_sec, eta_s,
	// current_file}.
	EventProgress = "download:progress"
	// EventStateChanged fires on every state transition (including
	// pending_approval->queued, queued->running, running->paused,
	// verifying, registering, done, failed, cancelled):
	// {job_id, state, error}.
	EventStateChanged = "download:state_changed"
	// EventDone fires once, when registration succeeds:
	// {job_id, config_id, model_id}.
	EventDone = "download:done"
	// EventFailed fires once, when the job gives up for good (bounded
	// retries exhausted, or a non-retryable error): {job_id, error}.
	EventFailed = "download:failed"
	// EventDeleted fires once, when a terminal job's row is dismissed from
	// the tray: {job_id}.
	EventDeleted = "download:deleted"
)
