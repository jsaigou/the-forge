// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_icons.go — the shared icon-upload handler (genealogies, families,
// models, configs) plus the four per-subject serve endpoints and their
// content-type helpers (split from catalog_handlers.go, Sprint 5
// code-quality cleanup, #33).

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	dark := r.URL.Query().Get("variant") == "dark"
	if err := r.ParseMultipartForm(maxIconSize); err != nil {
		writeValidationErrorCodes(w, map[string]string{"file": "must be ≤1 MB"}, map[string]string{"file": "too_large"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeValidationErrorCodes(w, map[string]string{"file": "is required (multipart field 'file')"}, map[string]string{"file": "required"})
		return
	}
	defer file.Close()

	ct := header.Header.Get("Content-Type")
	if ct == "" {
		ext := strings.ToLower(filepath.Ext(header.Filename))
		ct = extToContentType(ext)
	}
	if !allowedIconTypes[ct] {
		writeValidationErrorCodes(w, map[string]string{"file": "must be WebM, JPG, PNG, WebP, or SVG"}, map[string]string{"file": "invalid_file_type"})
		return
	}

	// Read the file content (already capped at maxIconSize by ParseMultipartForm).
	// io.ReadAll, not a single Read: Read is not guaranteed to fill the buffer.
	data, err := io.ReadAll(file)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "failed to read icon")
		return
	}

	// The declared type is client-supplied; the bytes must actually be that type (and an SVG must carry no active content), or a ".html" payload
	// with an image/png header would be stored and later served as a page.
	if !iconBytesMatch(ct, data) {
		writeValidationErrorCodes(w, map[string]string{"file": "content does not match its declared type"}, map[string]string{"file": "invalid_file_type"})
		return
	}

	// Save to IconsDir if configured, else store as a data URL in the logo field.
	// The dark variant gets its own filename suffix so it never collides
	// with the light file for the same subject/id.
	var logo string
	iconsDir := s.deps.Config().Paths.IconsDir
	if iconsDir != "" {
		// The extension comes from the validated type, never from the client's file name.
		ext := contentTypeToExt(ct)
		suffix := ""
		if dark {
			suffix = "-dark"
		}
		filename := fmt.Sprintf("%s-%d%s%s", subject, id, suffix, ext)
		dst := filepath.Join(iconsDir, filename)
		if err := os.MkdirAll(iconsDir, 0o755); err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "failed to create icons dir")
			return
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "failed to write icon")
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
			writeErrorCode(w, http.StatusNotFound, "not_found_detail", map[string]any{"detail": notFoundMsg}, notFoundMsg)
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
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "model", "model not found", cat.SetModelLogo, cat.SetModelLogoDark)
}

func (s *Server) handleCatalogFamilyIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "family", "family not found", cat.SetFamilyLogo, cat.SetFamilyLogoDark)
}

func (s *Server) handleCatalogGenealogyIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "genealogy", "genealogy not found", cat.SetGenealogyLogo, cat.SetGenealogyLogoDark)
}

func (s *Server) handleCatalogConfigIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
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

// svgAllowedElements is the drawing subset an icon needs. Anything else (script, foreignObject, style, animate/set, image, iframe, a, ...) is
// refused rather than stripped: an allowlist cannot be outflanked by a spelling the denylist did not think of.
var svgAllowedElements = map[string]bool{
	"svg": true, "g": true, "defs": true, "path": true, "circle": true, "ellipse": true, "rect": true, "line": true, "polyline": true, "polygon": true,
	"lineargradient": true, "radialgradient": true, "stop": true, "clippath": true, "mask": true, "pattern": true, "symbol": true, "use": true,
	"title": true, "desc": true, "text": true, "tspan": true, "metadata": true,
}

var svgURLRefRE = regexp.MustCompile(`(?i)url\(\s*['"]?\s*([^)'"\s]*)`)

// svgSafe reports whether data is a well-formed SVG made only of allowlisted elements, with no event handler, no reference that leaves the
// document and no entity/DOCTYPE tricks. encoding/xml decodes character references before this sees the value, so "&#106;avascript:" is
// checked as "javascript:". Any href must be a "#fragment" of the same document; url(...) in any attribute must be too.
func svgSafe(data []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.Entity = nil // only the five predefined entities; anything else is a parse error
	sawSVG := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return sawSVG
		}
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case xml.Directive: // <!DOCTYPE ...>, <!ENTITY ...>
			return false
		case xml.ProcInst:
			if !strings.EqualFold(t.Target, "xml") {
				return false // e.g. <?xml-stylesheet ...?>
			}
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if !svgAllowedElements[name] {
				return false
			}
			if name == "svg" {
				sawSVG = true
			}
			for _, a := range t.Attr {
				attr := strings.ToLower(a.Name.Local)
				if strings.HasPrefix(attr, "on") {
					return false
				}
				val := strings.TrimSpace(a.Value)
				if attr == "href" && !strings.HasPrefix(val, "#") {
					return false
				}
				lv := strings.ToLower(val)
				if attr == "src" || strings.Contains(lv, "javascript:") || strings.Contains(lv, "data:") || strings.Contains(lv, "expression(") || strings.Contains(lv, "@import") {
					return false
				}
				for _, m := range svgURLRefRE.FindAllStringSubmatch(val, -1) {
					if !strings.HasPrefix(m[1], "#") {
						return false
					}
				}
			}
		}
	}
}

// iconBytesMatch reports whether data really is the allowlisted type ct names. Raster and WebM types are checked by magic number (net/http's
// sniffer); an SVG must start like an SVG document and contain no script, event handler, foreignObject or external-entity construct.
func iconBytesMatch(ct string, data []byte) bool {
	sniffed := http.DetectContentType(data)
	switch ct {
	case "image/png", "image/jpeg", "image/webp":
		return sniffed == ct
	case "video/webm":
		// DetectContentType knows WebM by its EBML header.
		return sniffed == "video/webm"
	case "image/svg+xml":
		return svgSafe(data)
	}
	return false
}
