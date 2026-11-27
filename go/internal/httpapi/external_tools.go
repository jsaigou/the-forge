// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
)

// ── GET/PUT /api/v1/external-tools — infra.external_tools (live) ───────────
//
// Link-only Console entries for tools that run elsewhere, such as voice-studio and talk-studio on Core. Unlike a service row they have no unit, no port
// and no state here: the Console shows a name, an icon and a ↗ link, and draws no status it is not checking. The list is operator data (no names or
// addresses in the source), replaced whole by PUT, and takes effect without a restart, like infra.service_links.

const (
	maxExternalTools      = 24
	maxExternalToolName   = 64
	maxExternalToolURL    = 512
	maxExternalToolIcon   = 64
	maxExternalToolDetail = 200
)

var externalToolIcon = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type externalToolsResponse struct {
	Tools []config.ExternalTool `json:"tools"`
}

func (s *Server) resolvedExternalTools(ctx context.Context) []config.ExternalTool {
	var tools []config.ExternalTool
	if raw := s.getRawSetting(ctx, "infra.external_tools"); len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			log.Printf("httpapi: warning: corrupt stored setting infra.external_tools: %v", err)
		}
	}
	if tools == nil {
		tools = []config.ExternalTool{}
	}
	return tools
}

func (s *Server) handleExternalToolsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, externalToolsResponse{Tools: s.resolvedExternalTools(r.Context())})
}

type externalToolsBody struct {
	Tools []config.ExternalTool `json:"tools"`
}

// cleanExternalTools trims and validates the list. Field keys are "tools[<index>].<field>"; a nil result with empty maps means valid.
func cleanExternalTools(in []config.ExternalTool) (out []config.ExternalTool, fields, codes map[string]string) {
	fields, codes = map[string]string{}, map[string]string{}
	bad := func(key, msg, code string) {
		if _, seen := fields[key]; !seen {
			fields[key], codes[key] = msg, code
		}
	}
	if len(in) > maxExternalTools {
		bad("tools", fmt.Sprintf("at most %d entries", maxExternalTools), "too_many_items")
		return nil, fields, codes
	}
	seen := map[string]bool{}
	out = make([]config.ExternalTool, 0, len(in))
	for i, t := range in {
		key := func(f string) string { return fmt.Sprintf("tools[%d].%s", i, f) }
		t.Name, t.URL, t.Icon, t.Description = strings.TrimSpace(t.Name), strings.TrimSpace(t.URL), strings.TrimSpace(t.Icon), strings.TrimSpace(t.Description)
		switch {
		case t.Name == "":
			bad(key("name"), "must not be empty", "must_not_be_empty")
		case len(t.Name) > maxExternalToolName:
			bad(key("name"), fmt.Sprintf("at most %d characters", maxExternalToolName), "too_large")
		case seen[strings.ToLower(t.Name)]:
			bad(key("name"), "another entry already has this name", "already_exists")
		}
		seen[strings.ToLower(t.Name)] = true
		if u, err := url.Parse(t.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(t.URL) > maxExternalToolURL {
			bad(key("url"), "must be a full web address starting with http:// or https://", "invalid_url")
		}
		if t.Icon != "" && (len(t.Icon) > maxExternalToolIcon || !externalToolIcon.MatchString(t.Icon)) {
			bad(key("icon"), "must be an icon name: lowercase letters, digits, '.', '_' or '-'", "invalid_value")
		}
		if len(t.Description) > maxExternalToolDetail {
			bad(key("description"), fmt.Sprintf("at most %d characters", maxExternalToolDetail), "too_large")
		}
		out = append(out, t)
	}
	if len(fields) > 0 {
		return nil, fields, codes
	}
	return out, nil, nil
}

// handleExternalToolsPut — PUT /api/v1/external-tools (admin, page.settings). Replaces the whole list: send the full current list back with the change.
func (s *Server) handleExternalToolsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "settings"}, "settings store not wired")
		return
	}
	var body externalToolsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if body.Tools == nil {
		writeValidationErrorCodes(w, map[string]string{"tools": "must be present"}, map[string]string{"tools": "must_be_present"})
		return
	}
	cleaned, fields, codes := cleanExternalTools(body.Tools)
	if len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(cleaned)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, "infra.external_tools", raw); err != nil {
		writeInternalError(w, err)
		return
	}
	if s.deps.ReloadConfig != nil {
		s.deps.ReloadConfig()
	}
	s.audit(r, identity(r).Name, "external_tools", "infra.external_tools", string(raw))
	writeJSON(w, http.StatusOK, externalToolsResponse{Tools: s.resolvedExternalTools(ctx)})
}
