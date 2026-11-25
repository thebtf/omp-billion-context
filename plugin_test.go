package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testPlugin(t *testing.T, sidecarURL string, call hostCall) *plugin {
	t.Helper()
	env := map[string]string{
		"OMP_BILI_UPSTREAM_URL": "https://example.invalid/gateway",
		"OMP_BILI_MODEL_ALIAS":  "bili-opt-in", "OMP_BILI_TARGET_MODEL": "real-model",
		"OMP_BILI_CLIENT_TOKEN": "client-secret", "OMP_BILI_UPSTREAM_TOKEN": "upstream-secret",
	}
	cfg, err := readConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := url.Parse(sidecarURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.sidecar = *sidecar
	p := newPlugin(cfg, call)
	t.Cleanup(p.shutdown)
	return p
}

func invoke(t *testing.T, p *plugin, method string, request any) (any, *rpcError) {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return p.handle(method, payload)
}

func TestModelOptIn(t *testing.T) {
	p := testPlugin(t, "http://127.0.0.1:8787", nil)
	for _, tc := range []struct {
		model  string
		format protocol
		want   bool
	}{
		{"bili-opt-in", chat, true},
		{"bili-opt-in", responses, true},
		{"bili-opt-in", "claude", false},
		{"real-model", chat, false},
		{"bili-opt-in(extra)", chat, false},
		{"Bili-opt-in", chat, false},
	} {
		got, failure := invoke(t, p, "model.route", routeRequest{RequestedModel: tc.model, SourceFormat: tc.format})
		if failure != nil {
			t.Fatal(failure.Message)
		}
		decision := got.(routeResponse)
		if decision.Handled != tc.want || (tc.want && decision.TargetKind != "self") {
			t.Errorf("route %s/%s = %+v", tc.model, tc.format, decision)
		}
	}
}

func TestAlternateEndpointsFallThrough(t *testing.T) {
	p := testPlugin(t, "http://127.0.0.1:8787", nil)
	for _, wire := range []string{
		`{"RequestedModel":"bili-opt-in","SourceFormat":"openai","Metadata":{"request_path":"/v1/completions"}}`,
		`{"RequestedModel":"bili-opt-in","SourceFormat":"openai-response","Metadata":{"request_path":"/v1/responses/compact"}}`,
		`{"RequestedModel":"bili-opt-in","SourceFormat":"openai-response","Headers":{"Upgrade":["websocket"]}}`,
	} {
		decision, fail := p.handle("model.route", []byte(wire))
		if fail != nil || decision.(routeResponse).Handled {
			t.Fatalf("unexpected route for %s: %+v, %+v", wire, decision, fail)
		}
	}
}

func requestFor(format protocol, sessionHeader, session string) executorRequest {
	return executorRequest{
		Model: "bili-opt-in", SourceFormat: format, Format: format,
		Payload: []byte(`{"model":"bili-opt-in","input":"hello"}`),
		Headers: http.Header{
			"Authorization": {"Bearer client-secret"}, http.CanonicalHeaderKey(sessionHeader): {session},
			"Cookie": {"private"}, "X-Api-Key": {"other-private"}, "X-Bili-Plugin-Conversation": {"injected"},
		},
	}
}

func TestForwardingAndSessionIsolation(t *testing.T) {
	type observed struct {
		path, model, session, auth, cookie, apiKey, override, input string
	}
	requests := make(chan observed, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model, Input string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- observed{
			r.RequestURI, body.Model, r.Header.Get("X-Acp-Session"), r.Header.Get("Authorization"),
			r.Header.Get("Cookie"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Bili-Plugin-Conversation"), body.Input,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "must-not-forward")
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	p := testPlugin(t, server.URL, nil)
	for _, tc := range []struct {
		format                protocol
		header, session, path string
	}{
		{chat, "session_id", "thread-A", "/v1/chat/completions"},
		{responses, "x-acp-session", "thread-B", "/v1/responses"},
		{chat, "x-session-id", "thread-A", "/v1/chat/completions"},
	} {
		result, failure := invoke(t, p, "executor.execute", requestFor(tc.format, tc.header, tc.session))
		if failure != nil {
			t.Fatal(failure.Message)
		}
		resp := result.(executorResponse)
		if string(resp.Payload) != `{"ok":true}` || len(resp.Headers) != 1 || resp.Headers.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		select {
		case req := <-requests:
			want := observed{
				path: "/bili/https://example.invalid/gateway" + tc.path, model: "real-model", session: tc.session,
				auth: "Bearer upstream-secret", input: "hello",
			}
			if req != want {
				t.Errorf("sidecar got %+v; want %+v", req, want)
			}
		default:
			t.Fatal("sidecar did not receive request")
		}
	}

	bad := requestFor(chat, "x-acp-session", "thread-A")
	bad.Headers.Set("Authorization", "Bearer someone-else")
	if _, fail := invoke(t, p, "executor.execute", bad); fail == nil || fail.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("wrong-token error = %+v", fail)
	}
	bad = requestFor(chat, "x-acp-session", "thread-A")
	bad.Headers.Set("Session_Id", "thread-B")
	if _, fail := invoke(t, p, "executor.execute", bad); fail == nil || fail.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("conflicting-session error = %+v", fail)
	}
	bad = requestFor(chat, "x-acp-session", "")
	if _, fail := invoke(t, p, "executor.execute", bad); fail == nil || fail.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("missing-session error = %+v", fail)
	}
	if len(requests) != 0 {
		t.Fatal("rejected requests reached sidecar")
	}
}

func TestSidecarErrorsAreTypedAndRedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			followed.Store(true)
		}
		if r.Header.Get("X-Acp-Session") == "redirect" {
			http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
			return
		}
		http.Error(w, "private-error-body", http.StatusTooManyRequests)
	}))
	defer server.Close()
	p := testPlugin(t, server.URL, nil)
	for session, status := range map[string]int{"redirect": http.StatusBadGateway, "limit": http.StatusTooManyRequests} {
		_, fail := invoke(t, p, "executor.execute", requestFor(chat, "x-acp-session", session))
		if fail == nil || fail.HTTPStatus != status || strings.Contains(fail.Message, "private-error-body") {
			t.Errorf("%s error = %+v", session, fail)
		}
	}
	if followed.Load() {
		t.Fatal("sidecar redirect was followed")
	}
}

func TestLiveStreamFraming(t *testing.T) {
	for _, tc := range []struct {
		format                  protocol
		initial, terminal, want string
	}{
		{chat, "data: {\"delta\":1}\r\n\r\n", "data: [DONE]\n\n", `{"delta":1}`},
		{responses, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n", "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.initial)
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = fmt.Fprint(w, tc.terminal)
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			emitted := make(chan streamEmit, 2)
			closed := make(chan streamClose, 1)
			p := testPlugin(t, server.URL, func(method string, payload any) bool {
				switch method {
				case "host.stream.emit":
					chunk := payload.(streamEmit)
					chunk.Payload = append([]byte(nil), chunk.Payload...)
					emitted <- chunk
				case "host.stream.close":
					closed <- payload.(streamClose)
				default:
					return false
				}
				return true
			})
			req := requestFor(tc.format, "x-acp-session", "thread-A")
			req.Stream, req.StreamID = true, "7"
			if _, fail := invoke(t, p, "executor.execute_stream", req); fail != nil {
				t.Fatal(fail.Message)
			}
			select {
			case first := <-emitted:
				if first.StreamID != "7" || string(first.Payload) != tc.want {
					t.Fatalf("first chunk = %+v, payload %q", first, first.Payload)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first event did not reach host before upstream completion")
			}
			close(release)
			select {
			case last := <-closed:
				if last.StreamID != "7" || last.Error != "" {
					t.Fatalf("stream close = %+v", last)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("host stream was not closed")
			}
			if tc.format == responses {
				if len(emitted) != 1 || string((<-emitted).Payload) != tc.terminal {
					t.Fatal("Responses terminal event was not forwarded")
				}
			} else if len(emitted) != 0 {
				t.Fatal("Chat Completions terminal sentinel should be written by CPA")
			}
		})
	}
}
