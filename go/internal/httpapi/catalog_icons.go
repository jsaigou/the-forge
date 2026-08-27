// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_icons.go — the shared icon-upload handler (genealogies, families,
// models, configs) plus the four per-subject serve endpoints and their
// content-type helpers (split from catalog_handlers.go, Sprint 5
// code-quality cleanup, #33).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var allowedIconTypes = map[string]bool{
	"image/webp": true, "image/png": true, "image/jpeg": true,
	"image/svg+xml": true, "video/webm": true,
}

const maxIconSize = 1 << 20 // 1 MB

// handleCatalogIconUpload is the shared implementation. subject names the
// on-disk filename prefix and audit action ("model", "family", "genealogy",
// "config"); setLogo/setLogoDark persist the resolved logo value for id —
// which one runs is chosen by the request's ?variant=light|dark query param
// (default light) so the two existing single-purpose setters (Set*Logo,
// Set*LogoDark) don't need a combined-write variant of their own.
func (s *Server) handleCatalogIconUpload(w http.ResponseWriter, r *http.Request, subject, notFoundMsg string, setLogo, setLogoDark func(ctx context.Context, id int64, logo string) error) {
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	dark := r.URL.Query().Get("variant") == "dark"
	if err := r.ParseMultipartForm(maxIconSize); err != nil {
		writeValidationError(w, map[string]string{"file": "must be ≤1 MB"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeValidationError(w, map[string]string{"file": "is required (multipart field 'file')"})
		return
	}
	defer file.Close()

	ct := header.Header.Get("Content-Type")
	if ct == "" {
		ext := strings.ToLower(filepath.Ext(header.Filename))
		ct = extToContentType(ext)
	}
	if !allowedIconTypes[ct] {
		writeValidationError(w, map[string]string{"file": "must be WebM, JPG, PNG, WebP, or SVG"})
		return
	}

	// Read the file content (already capped at maxIconSize by ParseMultipartForm).
	// io.ReadAll, not a single Read: Read is not guaranteed to fill the buffer.
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read icon")
		return
	}

	// Save to IconsDir if configured, else store as a data URL in the logo field.
	// The dark variant gets its own filename suffix so it never collides
	// with the light file for the same subject/id.
	var logo string
	iconsDir := s.deps.Config().Paths.IconsDir
	if iconsDir != "" {
		ext := filepath.Ext(header.Filename)
		if ext == "" {
			ext = contentTypeToExt(ct)
		}
		suffix := ""
		if dark {
			suffix = "-dark"
		}
		filename := fmt.Sprintf("%s-%d%s%s", subject, id, suffix, ext)
		dst := filepath.Join(iconsDir, filename)
		if err := os.MkdirAll(iconsDir, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create icons dir")
			return
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to write icon")
			return
		}
		logo = filename
	} else {
		logo = "data:" + ct + ";base64," + base64Encode(data)
	}

	ctx, cancel := catalogCtx(r)
	defer cancel()
	set := setLogo
	auditSuffix := ""
	if dark {
		set = setLogoDark
		auditSuffix = "_dark"
	}
	if err := set(ctx, id, logo); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, notFoundMsg)
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_"+subject+"_icon"+auditSuffix, strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	key := "logo"
	if dark {
		key = "logo_dark"
	}
	writeJSON(w, http.StatusOK, map[string]string{key: logo})
}

func (s *Server) handleCatalogModelIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "model", "model not found", cat.SetModelLogo, cat.SetModelLogoDark)
}

func (s *Server) handleCatalogFamilyIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "family", "family not found", cat.SetFamilyLogo, cat.SetFamilyLogoDark)
}

func (s *Server) handleCatalogGenealogyIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "genealogy", "genealogy not found", cat.SetGenealogyLogo, cat.SetGenealogyLogoDark)
}

func (s *Server) handleCatalogConfigIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "config", "config not found", cat.SetConfigLogo, cat.SetConfigLogoDark)
}

func extToContentType(ext string) string {
	switch ext {
	case ".webp":
		return "image/webp"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".svg":
		return "image/svg+xml"
	case ".webm":
		return "video/webm"
	}
	return ""
}

func contentTypeToExt(ct string) string {
	switch ct {
	case "image/webp":
		return ".webp"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/svg+xml":
		return ".svg"
	case "video/webm":
		return ".webm"
	}
	return ".bin"
}

func base64Encode(data []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(data); i += 3 {
		b1 := data[i]
		var b2, b3 byte
		has2, has3 := i+1 < len(data), i+2 < len(data)
		if has2 {
			b2 = data[i+1]
		}
		if has3 {
			b3 = data[i+2]
		}
		sb.WriteByte(chars[b1>>2])
		sb.WriteByte(chars[((b1&0x03)<<4)|(b2>>4)])
		if has2 {
			sb.WriteByte(chars[((b2&0x0f)<<2)|(b3>>6)])
		} else {
			sb.WriteByte('=')
		}
		if has3 {
			sb.WriteByte(chars[b3&0x3f])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}
