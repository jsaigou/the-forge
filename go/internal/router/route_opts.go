// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"strings"
)

// routeOpts lets a handler other than chatCompletions reuse tryBackends (resolution, on-demand load, health gate,
// slot attribution, audit) without a second copy of it. It travels in the request context so tryBackends' signature,
// and its recursion on failover, stay as they were; the zero value (nothing in ctx) is exactly the chat behavior.
type routeOpts struct {
	// upstreamPath is appended to the backend's OpenAI base URL ("" = "/chat/completions").
	upstreamPath string
	// rootPath makes upstreamPath relative to the llama-server root instead of its OpenAI base (".../v1"). llama-server's
	// /tts lives at the root, not under /v1.
	rootPath bool
	// rawBody, when non-nil, is forwarded byte-for-byte instead of the re-serialized, wire-model-rewritten chat body.
	// /v1/systemone sets it: its body is not a chat body, and llama-server ignores its `model` field anyway.
	rawBody []byte
	// noSubstitute turns capability-tier substitution off. A decision model is not interchangeable with another model
	// of the same tier: a substitute that cannot answer the request is worse than an honest error.
	noSubstitute bool
	// passUpstreamErrors streams an upstream 5xx to the client instead of treating it as "this backend is broken,
	// fail over". For a request-shaped failure (e.g. systemone's "input is too large for the batch") a 502
	// all_backends_unavailable would hide the real reason.
	passUpstreamErrors bool
	// auditAction names the audit row ("" = "chat_completion").
	auditAction string
	// onSuccessBody, when set, wraps a committed 2xx body so the handler can read what the upstream said (the
	// token usage) once the response has been fully sent.
	onSuccessBody func(buf []byte)
}

// target joins the opts' path onto a backend base URL (an OpenAI base, normally ending in /v1).
func (o routeOpts) target(base string) string {
	if o.rootPath {
		base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	}
	return base + o.path()
}

type routeOptsKey struct{}

func withRouteOpts(ctx context.Context, o routeOpts) context.Context {
	return context.WithValue(ctx, routeOptsKey{}, o)
}

func routeOptsFrom(ctx context.Context) routeOpts {
	o, _ := ctx.Value(routeOptsKey{}).(routeOpts)
	return o
}

func (o routeOpts) path() string {
	if o.upstreamPath == "" {
		return "/chat/completions"
	}
	return o.upstreamPath
}

func (o routeOpts) action() string {
	if o.auditAction == "" {
		return "chat_completion"
	}
	return o.auditAction
}
