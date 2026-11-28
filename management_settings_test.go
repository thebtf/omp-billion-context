package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestDashboardWritesAreLimitedToAuthenticatedMarkerSettings(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	resource := "/v0/resource/plugins/" + pluginID + webUIResource
	api := "/v0/management" + webUIRoute
	for _, tt := range []struct {
		method, path, target, body string
		status                     int
	}{
		{http.MethodPut, resource, "/__bili/settings", `{"visibilityMarkers":false}`, 405},
		{http.MethodPost, api, "/__bili/settings", `{"visibilityMarkers":false}`, 405},
		{http.MethodPut, api, "/__bili/overview", `{"visibilityMarkers":false}`, 405},
		{http.MethodPut, api, "/__bili/settings", `{"visibilityMarkers":null}`, 400},
		{http.MethodPut, api, "/__bili/settings", `{"visibilityMarkers":"false"}`, 400},
		{http.MethodPut, api, "/__bili/settings", `{"visibilityMarkers":false,"visibilityMarkers":true}`, 400},
		{http.MethodPut, api, "/__bili/settings", `{"visibilityMarkers":false,"priority":100}`, 400},
		{http.MethodGet, api, "/__bili/sessions/%2e%2e/detail", "", 400},
		{http.MethodGet, api, "/__bili/sessions?page=1&page=2", "", 400},
	} {
		raw, err := json.Marshal(managementRequest{Method: tt.method, Path: tt.path, Query: url.Values{"target": {tt.target}}, Body: []byte(tt.body)})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.handleManagement(raw); got.StatusCode != tt.status {
			t.Errorf("%s %s %s: got %d, want %d", tt.method, tt.path, tt.target, got.StatusCode, tt.status)
		}
	}
}
