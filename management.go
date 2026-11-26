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
	webUITimeout   = 10 * time.Second
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
		return managementError(http.StatusMethodNotAllowed, "Use the standalone bili WebUI to make changes")
	}
	switch req.Path {
	case "/v0/resource/plugins/" + pluginID + webUIResource:
		return newManagementResponse(http.StatusOK, "text/html; charset=utf-8", webUIShell)
	case "/v0/management" + webUIRoute:
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
	"overview":        nil,
	"sessions":        {"page", "pageSize", "q", "protocol", "client", "state"},
	"session-report":  {"session", "format", "mask", "lang"},
	"cache-report":    {"session"},
	"diagnostics":     {"topic"},
	"stats":           nil,
	"status":          nil,
	"config":          nil,
	"upstream":        nil,
	"logs":            {"q", "lines", "ctx", "win", "raw", "all"},
	"pricing":         nil,
	"resign":          nil,
	"packs":           nil,
	"packs/templates": nil,
	"update/state":    nil,
	"changelog":       nil,
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
		if len(parts) == 3 {
			switch {
			case parts[0] == "sessions" && webUIName.MatchString(parts[1]):
				allowed, ok = sessionReads[parts[2]]
			case parts[0] == "packs" && parts[1] == "user" && webUIName.MatchString(parts[2]) && !strings.Contains(parts[2], ".."):
				ok = true
			}
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
	running, _, err := p.runningEngine()
	if err != nil {
		return managementError(http.StatusServiceUnavailable, "Could not start bundled billion-context")
	}
	ctx, cancel := context.WithTimeout(p.ctx, webUITimeout)
	defer cancel()
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
	case target.Path != biliWebUIRoot && (mediaType == "application/json" || (target.Path == biliWebUIRoot+"logs" && mediaType == "text/plain")) &&
		(response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode >= 400 && response.StatusCode < 600):
	default:
		return managementError(http.StatusBadGateway, "Unexpected response from bundled billion-context")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, webUIReadLimit+1))
	if err != nil || len(body) > webUIReadLimit {
		return managementError(http.StatusBadGateway, "Bundled billion-context response could not be read in full")
	}
	result := newManagementResponse(response.StatusCode, contentType, body)
	if target.Path == biliWebUIRoot+"logs" {
		if incomplete := response.Header.Get("X-Bili-Log-Incomplete"); incomplete != "" {
			result.Headers.Set("X-Bili-Log-Incomplete", incomplete)
		}
	}
	return result
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
