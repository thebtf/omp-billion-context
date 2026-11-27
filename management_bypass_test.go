package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestWithBypassSummaryKeepsBiliPayloadIntact(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	p.skips.record(missingSessionID, time.Unix(0, 0))
	p.skips.record(otherEndpoint, time.Unix(0, 0))

	out := p.withBypassSummary([]byte(`{"version":"0.1.216","overview":{"requests":4},"notices":[]}`))
	var decoded struct {
		Version        string          `json:"version"`
		Overview       map[string]any  `json:"overview"`
		Notices        []any           `json:"notices"`
		CPACompression json.RawMessage `json:"cpaCompression"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if decoded.Version != "0.1.216" || decoded.Overview["requests"] != float64(4) || decoded.Notices == nil {
		t.Fatalf("bili fields changed: %s", out)
	}
	var summary struct {
		Counts       map[string]uint64 `json:"counts"`
		Uncompressed uint64            `json:"uncompressed"`
	}
	if err := json.Unmarshal(decoded.CPACompression, &summary); err != nil {
		t.Fatalf("summary is missing: %s", out)
	}
	if summary.Counts["missing_session_id"] != 1 || summary.Counts["other_endpoint"] != 1 {
		t.Fatalf("counts = %v", summary.Counts)
	}
	if summary.Uncompressed != 1 {
		t.Fatalf("uncompressed = %d, want only actionable bypasses", summary.Uncompressed)
	}
}

func TestWithBypassSummaryLeavesUnknownPayloadsAlone(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	for _, body := range []string{`[]`, `not json`, `null`} {
		if got := string(p.withBypassSummary([]byte(body))); got != body {
			t.Fatalf("payload rewritten: %q", got)
		}
	}
}

func TestBypassSummaryRouteReturnsCounters(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	p.skips.record(missingCallerScope, time.Unix(0, 0))
	raw, err := json.Marshal(managementRequest{Method: http.MethodGet, Path: "/v0/management" + webUIRoute, Query: map[string][]string{"target": {innerRoute}}})
	if err != nil {
		t.Fatal(err)
	}
	response := p.handleManagement(raw)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}
	var summary struct {
		Counts       map[string]uint64 `json:"counts"`
		Uncompressed uint64            `json:"uncompressed"`
	}
	if json.Unmarshal(response.Body, &summary) != nil || summary.Counts["missing_caller_scope"] != 1 || summary.Uncompressed != 1 {
		t.Fatalf("body = %s", response.Body)
	}
}

func TestBypassRouteRejectsExtraQueryParameters(t *testing.T) {
	p := newPlugin(nil)
	defer p.shutdown()
	raw, err := json.Marshal(managementRequest{Method: http.MethodGet, Path: "/v0/management" + webUIRoute, Query: map[string][]string{"target": {innerRoute}, "x": {"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response := p.handleManagement(raw); response.StatusCode == http.StatusOK {
		t.Fatalf("unexpected success: %s", response.Body)
	}
}
