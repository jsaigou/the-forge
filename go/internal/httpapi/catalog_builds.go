// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_builds.go — the builds write API (ADR-0020 F8, docs/api-contract-implementation-plan.md phase 4).
//
// A build is one llama-server binary a config runs on. Until now builds could only be written by smith's
// build_refresh and by SQL migrations, so registering or retiring one needed a daemon deploy. These routes are the
// same guards smith applies, reachable by an admin: the binary must be an absolute path under an allowed root, exist
// and be executable; a build is not retired while a config still runs on it; and the response says which load-mode
// flag spelling the binary takes.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/buildguard"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/store"
)

// SettingCatalogBuildRoots ("catalog.build_roots") is the JSON array of directories a build's binary may live under.
const SettingCatalogBuildRoots = "catalog.build_roots"

// probeBuildLoadMode is a variable so tests do not have to execute a real llama-server.
var probeBuildLoadMode = hfdownload.ProbeLoadMode

const retiredPrefix = "(retired) "

type buildBody struct {
	EngineID   int64  `json:"engine_id"`
	Name       string `json:"name"`
	BinaryPath string `json:"binary_path"`
	Backend    string `json:"backend"`
	Reason     string `json:"reason"`
	Retired    bool   `json:"retired"`
	// LaunchEnv is the environment added to the engine process (validated: names, values, paths under the build roots or
	// catalog.launch_env_roots, no secrets, size caps). Like every field of this body it is a full replace: omitting it clears it.
	LaunchEnv map[string]string `json:"launch_env"`
}

// buildRoots returns the allowed roots: the catalog.build_roots setting, or the defaults when it is not set. A setting that exists but cannot be
// read is an error, not a reason to fall back to the defaults (they can be wider than what the operator configured).
func (s *Server) buildRoots(ctx context.Context) ([]string, error) {
	var configured []string
	if s.deps.Settings != nil {
		raw, err := s.deps.Settings.Get(ctx, SettingCatalogBuildRoots)
		switch {
		case err == nil:
			_ = json.Unmarshal(raw, &configured)
		case errors.Is(err, store.ErrNotFound):
		default:
			return nil, err
		}
	}
	return buildguard.Roots(configured), nil
}

// checkBuildBinary is the build guard over the configured roots (ADR-0020 F8): what is wrong with a build's binary path, "" when it is fine.
func (s *Server) checkBuildBinary(ctx context.Context, path string) (string, error) {
	roots, err := s.buildRoots(ctx)
	if err != nil {
		return "", err
	}
	return buildguard.CheckBinary(path, roots), nil
}

// buildWrites builds the Build write module from the live dependencies: every create, update, retire and delete of a Build goes through it.
func (s *Server) buildWrites() store.BuildWrites {
	return store.BuildWrites{Cat: s.deps.Catalog, Changed: s.invalidateCfg, CheckBinary: s.checkBuildBinary, CheckLaunchEnv: s.checkBuildLaunchEnv}
}

// launchEnvRoots reads catalog.launch_env_roots: directories besides the build roots that a path in a launch_env may live under (host
// libraries such as a ROCm tree). Unset means none. A setting that exists but cannot be read is an error, not "none".
func (s *Server) launchEnvRoots(ctx context.Context) ([]string, error) {
	if s.deps.Settings == nil {
		return nil, nil
	}
	raw, err := s.deps.Settings.Get(ctx, buildguard.SettingLaunchEnvRoots)
	switch {
	case err == nil:
		var roots []string
		if jerr := json.Unmarshal(raw, &roots); jerr != nil {
			return nil, jerr
		}
		return roots, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

// checkBuildLaunchEnv is buildguard.CheckLaunchEnv over the build roots plus the launch-environment roots.
func (s *Server) checkBuildLaunchEnv(ctx context.Context, env map[string]string) (map[string]string, error) {
	roots, err := s.buildRoots(ctx)
	if err != nil {
		return nil, err
	}
	extra, err := s.launchEnvRoots(ctx)
	if err != nil {
		return nil, err
	}
	return buildguard.CheckLaunchEnv(env, append(roots, buildguard.Roots(extra)...)), nil
}

// toStore is the one place a request body becomes a store.Build; the module normalizes it (legacy prefix, clean path, no path when retired).
func (b buildBody) toStore() store.Build {
	return store.Build{EngineID: b.EngineID, Name: b.Name, BinaryPath: b.BinaryPath, Backend: b.Backend, Reason: b.Reason, Retired: b.Retired, LaunchEnv: b.LaunchEnv}
}

// writeBuildError maps the Build write module's errors onto the API's codes.
func writeBuildError(w http.ResponseWriter, err error) {
	var inv *store.Invalid
	var inUse *store.BuildInUseError
	var notRetired *store.BuildNotRetiredError
	switch {
	case errors.As(err, &inv):
		writeValidationErrorCodes(w, inv.Fields, inv.Codes)
	case errors.As(err, &inUse):
		writeErrorCode(w, http.StatusConflict, "build_in_use",
			map[string]any{"count": len(inUse.Configs), "configs": strings.Join(inUse.Configs, ", ")}, inUse.Error())
	case errors.As(err, &notRetired):
		writeErrorCode(w, http.StatusConflict, "build_not_retired", map[string]any{"name": notRetired.Build}, notRetired.Error())
	case isNotFound(err):
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "build"}, "build not found")
	default:
		writeInternalError(w, err)
	}
}

// probeBuildGuarded reports whether a stored build's binary may be exec'd to ask it about --load-mode. The probe
// runs on a plain GET, and a row written before the build guard existed (a migration, a pre-F8 database) can name a
// path today's write API would refuse, so the same guard applies here. A rejected path — or a settings read that
// fails — simply leaves load_mode_supported out of the response.
func (s *Server) probeBuildGuarded(ctx context.Context, path string) bool {
	reason, err := s.checkBuildBinary(ctx, path)
	return err == nil && reason == ""
}

func (s *Server) buildResponse(ctx context.Context, b store.Build) buildJSON {
	out := buildToJSON(b)
	if !b.Retired && b.BinaryPath != "" && s.probeBuildGuarded(ctx, b.BinaryPath) {
		if supported, known := probeBuildLoadMode(ctx, b.BinaryPath); known {
			out.LoadModeSupported = &supported
		}
	}
	return out
}

func (s *Server) handleCatalogBuildGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	b, err := cat.GetBuild(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "build"}, "build not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildToJSON(b))
}

func (s *Server) handleCatalogBuildCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b buildBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	got, err := s.buildWrites().Create(ctx, b.toStore())
	if err != nil {
		writeBuildError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_build_create", strconv.FormatInt(got.ID, 10), got.Name)
	writeJSON(w, http.StatusCreated, s.buildResponse(ctx, got))
}

func (s *Server) handleCatalogBuildUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	var b buildBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	before, after, err := s.buildWrites().Update(ctx, id, b.toStore())
	if err != nil {
		writeBuildError(w, err)
		return
	}
	action := "catalog_build_update"
	if after.Retired && !before.Retired {
		action = "catalog_build_retire"
	}
	s.audit(r, identity(r).Name, action, strconv.FormatInt(id, 10), after.Name)
	writeJSON(w, http.StatusOK, s.buildResponse(ctx, after))
}

// handleCatalogBuildDelete removes a build row, for cleaning up a registration made by mistake or for a test. It is
// deliberately narrower than retire: only an already-retired build can be deleted (so a live one is never removed in
// one step), and never while a config still references it. Retired rows that are real history are kept, not deleted.
func (s *Server) handleCatalogBuildDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	b, err := s.buildWrites().Delete(ctx, id)
	if err != nil {
		writeBuildError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_build_delete", strconv.FormatInt(id, 10), b.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogBuildValidate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b buildBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	inv, err := s.buildWrites().Validate(ctx, b.toStore(), validateExcludeID(r))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if inv != nil {
		writeValidationErrorCodes(w, inv.Fields, inv.Codes)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}
