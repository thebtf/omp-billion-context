package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	webUIResource  = "/webui"
	webUIRoute     = "/plugins/" + pluginID + "/webui-data"
	biliWebUIRoot  = "/__bili/"
	innerRoute     = "/bypass"
	webUITimeout   = 30 * time.Second
	webUIReadLimit = 8 << 20
)

//go:embed webui.html
var webUIShell []byte

type managementRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes"`
	Resources []managementResource `json:"resources"`
}

type managementRoute struct {
	Method string
	Path   string
}

type managementResource struct {
	Path        string
	Menu        string
	Description string
}

func registerManagement() managementRegistration {
	return managementRegistration{
		Routes:    []managementRoute{{Method: http.MethodGet, Path: webUIRoute}, {Method: http.MethodPut, Path: webUIRoute}},
		Resources: []managementResource{{Path: webUIResource, Menu: "Billion Context", Description: "Compression activity, sessions and chat notifications"}},
	}
}

func (p *plugin) handleManagement(raw []byte) managementResponse {
	var req managementRequest
	if json.Unmarshal(raw, &req) != nil {
		return managementError(http.StatusBadRequest, "Invalid management request")
	}
	switch req.Path {
	case "/v0/resource/plugins/" + pluginID + webUIResource:
		if req.Method != http.MethodGet {
			return managementError(http.StatusMethodNotAllowed, "Use GET to open the dashboard")
		}
		return newManagementResponse(http.StatusOK, "text/html; charset=utf-8", webUIShell)
	case "/v0/management" + webUIRoute:
	default:
		return managementError(http.StatusNotFound, "Unknown management route")
	}
	values := req.Query["target"]
	if len(req.Query) != 1 || len(values) != 1 {
		return managementError(http.StatusBadRequest, "Choose one dashboard resource")
	}
	if req.Method == http.MethodGet && values[0] == innerRoute {
		return p.bypassSummary()
	}
	target, ok := webUITarget(req.Query)
	if !ok {
		return managementError(http.StatusBadRequest, "Unknown dashboard resource")
	}
	if req.Method == http.MethodGet {
		return p.readWebUI(target)
	}
	if req.Method != http.MethodPut || target.Path != biliWebUIRoot+"settings" {
		return managementError(http.StatusMethodNotAllowed, "Only chat notifications can be changed here")
	}
	if len(req.Body) > 256 || !json.Valid(req.Body) {
		return managementError(http.StatusBadRequest, "Expected one boolean: visibilityMarkers")
	}
	settings := json.NewDecoder(bytes.NewReader(req.Body))
	opening, _ := settings.Token()
	key, _ := settings.Token()
	value, _ := settings.Token()
	visible, boolean := value.(bool)
	if opening != json.Delim('{') || key != "visibilityMarkers" || !boolean || settings.More() {
		return managementError(http.StatusBadRequest, "Expected one boolean: visibilityMarkers")
	}
	return p.dashboardOperation("configure", map[string]any{"visibilityMarkers": visible}, nil)
}

var webUIName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

func webUITarget(query url.Values) (*url.URL, bool) {
	values := query["target"]
	if len(query) != 1 || len(values) != 1 {
		return nil, false
	}
	raw := values[0]
	if len(raw) > 8192 || !strings.HasPrefix(raw, biliWebUIRoot) || strings.ContainsAny(raw, "#\r\n\t ") {
		return nil, false
	}
	target, err := url.ParseRequestURI(raw)
	if err != nil || target.Host != "" || target.IsAbs() || target.EscapedPath() != target.Path || target.ForceQuery {
		return nil, false
	}
	switch name := strings.TrimPrefix(target.Path, biliWebUIRoot); name {
	case "overview", "settings":
		return target, target.RawQuery == ""
	case "sessions":
		params, err := url.ParseQuery(target.RawQuery)
		if err != nil {
			return nil, false
		}
		for key, values := range params {
			if len(values) != 1 || (key != "page" && key != "pageSize" && key != "q") {
				return nil, false
			}
		}
		return target, true
	default:
		parts := strings.Split(name, "/")
		return target, len(parts) == 3 && parts[0] == "sessions" && webUIName.MatchString(parts[1]) && parts[2] == "detail" && target.RawQuery == ""
	}
}

func (p *plugin) readWebUI(target *url.URL) managementResponse {
	return p.dashboardOperation("dashboard", map[string]any{"target": target.RequestURI()}, target)
}

func (p *plugin) dashboardOperation(op string, fields map[string]any, target *url.URL) managementResponse {
	ctx, cancel := context.WithTimeout(p.ctx, webUITimeout)
	defer cancel()
	var body json.RawMessage
	if err := p.kernelCall(ctx, op, fields, &body); err != nil {
		return managementError(http.StatusServiceUnavailable, "Compression storage is unavailable. Try again.")
	}
	if len(body) == 0 || string(body) == "null" {
		return managementError(http.StatusNotFound, "Session not found")
	}
	if len(body) > webUIReadLimit {
		return managementError(http.StatusRequestEntityTooLarge, "Dashboard result is too large")
	}
	if target != nil {
		switch target.Path {
		case biliWebUIRoot + "overview":
			body = p.withBypassSummary(body)
		case biliWebUIRoot + "sessions":
			body = withReadableTitles(body)
		}
	}
	return newManagementResponse(http.StatusOK, "application/json", body)
}

// withReadableTitles rewrites only the display strings of a sessions page.
// Identifiers, counters and unknown fields pass through byte-identical.
func withReadableTitles(body []byte) []byte {
	var page map[string]json.RawMessage
	if json.Unmarshal(body, &page) != nil || page == nil {
		return body
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(page["sessions"], &rows) != nil {
		return body
	}
	changed := false
	for _, row := range rows {
		changed = readableRow(row) || changed
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(rows)
	if err != nil || len(encoded) > webUIReadLimit {
		return body
	}
	page["sessions"] = encoded
	merged, err := json.Marshal(page)
	if err != nil || len(merged) > webUIReadLimit {
		return body
	}
	return merged
}

// rowDisplayOrder is the order in which the bundled list picks the string it
// prints for a row: the title, then the label, then the first-turn hint.
var rowDisplayOrder = []string{"title", "label", "firstBlockHint"}

// rowNameSources are the fields that can name a session, most deliberate
// first: an operator's rename, then an explicit label, then the opener of the
// first turn. A title that only repeats harness scaffolding yields to these.
var rowNameSources = []string{"hostTitle", "label", "firstBlockHint"}

// rowTitleFields are every field of a session row that carries display text.
var rowTitleFields = []string{"title", "hostTitle", "label", "firstBlockHint"}

// readableRow rewrites the display strings of one session row in place and
// reports whether anything changed. Identifiers, counters and unknown fields
// are never touched, and a field is only written when its text really changes.
func readableRow(row map[string]json.RawMessage) bool {
	cleaned := make(map[string]string, len(rowTitleFields))
	blanked := make([]string, 0, 2)
	changed := false
	for _, field := range rowTitleFields {
		raw, ok := row[field]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" {
			continue
		}
		readable := sessionTitle(value)
		cleaned[field] = readable
		if readable != value && writeDisplayString(row, field, readable) {
			changed = true
		}
		if readable == "" {
			// A field that was nothing but wrapper tags is empty now, so the
			// row must not keep markup where a sibling names the work.
			blanked = append(blanked, field)
		}
	}
	// A field that was only tags is empty now, so a sibling that names the work
	// fills it. Then the field the list prints first must say something: when it
	// is the harness preamble, the name goes into `title` if that slot is free,
	// so the preamble keeps its date instead of being overwritten.
	best := ""
	for _, field := range rowNameSources {
		if value := cleaned[field]; usefulTitle(value) {
			best = value
			break
		}
	}
	if best == "" {
		return changed
	}
	for _, field := range blanked {
		changed = writeDisplayString(row, field, best) || changed
	}
	for _, field := range rowDisplayOrder {
		value, ok := cleaned[field]
		if !ok || value == "" {
			continue
		}
		if !usefulTitle(value) {
			// The printed field is the preamble: `title` is what the list reads
			// first, so the name goes there while the hint keeps its date.
			changed = writeDisplayString(row, "title", best) || changed
		}
		// Only the first non-empty value decides what the list prints.
		break
	}
	return changed
}

// writeDisplayString stores one display string in a row field and reports
// whether the encoded value differs from what was there.
func writeDisplayString(row map[string]json.RawMessage, field, value string) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if string(row[field]) == string(encoded) {
		return false
	}
	row[field] = encoded
	return true
}

// withBypassSummary attaches the plugin's own counters to bili's overview JSON.
// An unrecognized or oversized payload is returned untouched rather than lost.
func (p *plugin) withBypassSummary(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return body
	}
	counts, total, formats := p.skips.detail()
	summary, err := json.Marshal(map[string]any{"counts": counts, "uncompressed": total, "formats": formats, "protected_fallbacks": p.protectedFallbacks.Load()})
	if err != nil {
		return body
	}
	payload["cpaCompression"] = summary
	if recent, ok := payload["recent"]; ok {
		var rows []map[string]json.RawMessage
		if json.Unmarshal(recent, &rows) == nil {
			changed := false
			for _, row := range rows {
				changed = readableRow(row) || changed
			}
			if changed {
				payload["recent"], _ = json.Marshal(rows)
			}
		}
	}
	merged, err := json.Marshal(payload)
	if err != nil || len(merged) > webUIReadLimit {
		return body
	}
	return merged
}

func (p *plugin) bypassSummary() managementResponse {
	counts, total, formats := p.skips.detail()
	body, err := json.Marshal(map[string]any{"counts": counts, "uncompressed": total, "formats": formats, "protected_fallbacks": p.protectedFallbacks.Load()})
	if err != nil {
		return managementError(http.StatusInternalServerError, "Could not read bypass counters")
	}
	return newManagementResponse(http.StatusOK, "application/json", body)
}

func newManagementResponse(status int, contentType string, body []byte) managementResponse {
	return managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Cache-Control":          {"no-store"},
			"Content-Type":           {contentType},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}

func managementError(status int, message string) managementResponse {
	body, _ := json.Marshal(map[string]string{"error": message})
	return newManagementResponse(status, "application/json", body)
}
