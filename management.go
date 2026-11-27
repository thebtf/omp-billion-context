package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
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
		Routes: []managementRoute{{Method: http.MethodGet, Path: webUIRoute}},
		Resources: []managementResource{{
			Path: webUIResource, Menu: "Billion Context", Description: "Read-only WebUI for the bundled billion-context engine",
		}},
	}
}

func (p *plugin) handleManagement(raw []byte) managementResponse {
	var req managementRequest
	if json.Unmarshal(raw, &req) != nil {
		return managementError(http.StatusBadRequest, "Invalid management request")
	}
	if req.Method != http.MethodGet {
		return managementError(http.StatusMethodNotAllowed, "The CPA WebUI is read-only")
	}
	switch req.Path {
	case "/v0/resource/plugins/" + pluginID + webUIResource:
		return newManagementResponse(http.StatusOK, "text/html; charset=utf-8", webUIShell)
	case "/v0/management" + webUIRoute:
		if target := req.Query.Get("target"); target == innerRoute && len(req.Query) == 1 {
			return p.bypassSummary()
		}
		target, ok := webUITarget(req.Query)
		if !ok {
			return managementError(http.StatusBadRequest, "Unsupported billion-context read target")
		}
		return p.readWebUI(target)
	default:
		return managementError(http.StatusNotFound, "Unknown management route")
	}
}

var webUIReads = map[string][]string{
	"overview":       nil,
	"sessions":       {"page", "pageSize", "q", "protocol", "client", "state"},
	"session-report": {"session", "format", "mask", "lang"},
	"cache-report":   {"session"},
	"diagnostics":    {"topic"},
	"stats":          nil,
	"status":         nil,
}

var sessionReads = map[string][]string{
	"detail":        nil,
	"tree":          nil,
	"block-content": {"blockId", "view"},
	"message":       {"ref"},
}

var webUIName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	name := strings.TrimPrefix(target.Path, biliWebUIRoot)
	allowed, ok := webUIReads[name]
	if name == "" {
		allowed, ok = []string{"embed", "theme"}, true
	} else if !ok {
		parts := strings.Split(name, "/")
		if len(parts) == 3 && parts[0] == "sessions" && webUIName.MatchString(parts[1]) {
			allowed, ok = sessionReads[parts[2]]
		}
	}
	if !ok || (len(allowed) == 0 && target.RawQuery != "") {
		return nil, false
	}
	params, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return nil, false
	}
	for key, values := range params {
		if len(values) != 1 || !slices.Contains(allowed, key) {
			return nil, false
		}
	}
	if name == "" {
		theme := params.Get("theme")
		if params.Has("theme") && theme != "light" && theme != "dark" {
			return nil, false
		}
		params.Set("embed", "1")
		target.RawQuery = params.Encode()
	}
	return target, true
}

func (p *plugin) readWebUI(target *url.URL) managementResponse {
	ctx, cancel := context.WithTimeout(p.ctx, webUITimeout)
	defer cancel()
	running, _, err := p.runningEngine(ctx)
	if err != nil {
		failure := engineFailure(err)
		return managementError(failure.HTTPStatus, failure.Message)
	}
	destination := running.url
	destination.Path, destination.RawQuery = target.Path, target.RawQuery
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, destination.String(), nil)
	if err != nil {
		return managementError(http.StatusBadGateway, "Could not request bundled billion-context")
	}
	request.Header.Set("X-Omp-Bili-Access", running.token)
	response, err := running.client.Do(request)
	if err != nil {
		return managementError(http.StatusBadGateway, "Could not read bundled billion-context")
	}
	defer response.Body.Close()

	contentType := response.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case target.Path == biliWebUIRoot && response.StatusCode == http.StatusOK && mediaType == "text/html":
	case target.Path != biliWebUIRoot && mediaType == "application/json" &&
		(response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode >= 400 && response.StatusCode < 600):
	default:
		return managementError(http.StatusBadGateway, "Unexpected response from bundled billion-context")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, webUIReadLimit+1))
	if err != nil || len(body) > webUIReadLimit {
		return managementError(http.StatusBadGateway, "Bundled billion-context response could not be read in full")
	}
	if target.Path == biliWebUIRoot+"overview" && response.StatusCode == http.StatusOK {
		body = p.withBypassSummary(body)
	}
	if target.Path == biliWebUIRoot+"sessions" && response.StatusCode == http.StatusOK {
		body = withReadableTitles(body)
	}
	return newManagementResponse(response.StatusCode, contentType, body)
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

// readableRow rewrites the display strings of one session row in place and
// reports whether anything changed.
func readableRow(row map[string]json.RawMessage) bool {
	fields := []string{"title", "hostTitle", "firstBlockHint"}
	cleaned := make(map[string]string, len(fields))
	empty := make(map[string]bool, len(fields))
	changed := false
	for _, field := range fields {
		raw, ok := row[field]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" {
			continue
		}
		readable := sessionTitle(value)
		if readable == "" {
			// A field that was nothing but wrapper tags must not keep them;
			// clearing it lets an informative sibling name the row.
			empty[field] = true
			if encoded, err := json.Marshal(""); err == nil {
				row[field] = encoded
				changed = true
			}
			continue
		}
		cleaned[field] = readable
		if readable == value {
			continue
		}
		if encoded, err := json.Marshal(readable); err == nil {
			row[field] = encoded
			changed = true
		}
	}
	// The list shows title, then hostTitle, then the hint. A boilerplate first
	// choice hides a later field that actually names the work.
	var best string
	for _, field := range fields {
		if value := cleaned[field]; value != "" && !isBoilerplate(value) && !isGenericJob(value) {
			best = value
			break
		}
	}
	if best == "" {
		return changed
	}
	for _, field := range fields {
		if empty[field] {
			if encoded, err := json.Marshal(best); err == nil {
				row[field] = encoded
				changed = true
			}
			continue
		}
		value, ok := cleaned[field]
		if !ok {
			continue
		}
		if isBoilerplate(value) || isGenericJob(value) {
			if encoded, err := json.Marshal(best); err == nil {
				row[field] = encoded
				changed = true
			}
		}
		break
	}
	return changed
}

// withBypassSummary attaches the plugin's own counters to bili's overview JSON.
// An unrecognized or oversized payload is returned untouched rather than lost.
func (p *plugin) withBypassSummary(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return body
	}
	counts, total, formats := p.skips.detail()
	summary, err := json.Marshal(map[string]any{"counts": counts, "uncompressed": total, "formats": formats})
	if err != nil {
		return body
	}
	payload["cpaCompression"] = summary
	// The Overview page renders its session list from overview.recent, so that
	// array needs the same display cleanup as the Sessions page.
	if recent, ok := payload["overview"]; ok {
		var overview map[string]json.RawMessage
		if json.Unmarshal(recent, &overview) == nil {
			var rows []map[string]json.RawMessage
			if json.Unmarshal(overview["recent"], &rows) == nil {
				changed := false
				for _, row := range rows {
					changed = readableRow(row) || changed
				}
				if changed {
					if encoded, err := json.Marshal(rows); err == nil {
						overview["recent"] = encoded
						if reencoded, err := json.Marshal(overview); err == nil {
							payload["overview"] = reencoded
						}
					}
				}
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
	body, err := json.Marshal(map[string]any{"counts": counts, "uncompressed": total, "formats": formats})
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
