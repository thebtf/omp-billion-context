package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestOverflowRetriesOnlyAnExplicitContextRefusal(t *testing.T) {
	for _, c := range []struct {
		code, message string
		status        int
		overflow      bool
	}{
		{"context_length_exceeded", "limit", 400, true}, {"invalid_request", "maximum context length exceeded", 400, true}, {"request_too_large", "body size limit", 413, false}, {"invalid_key", "request failed", 401, false}, {"temporary", "context length service unavailable", 502, false},
	} {
		if got := isOverflow(kernelError(c.code, c.message, c.status)); got != c.overflow {
			t.Errorf("%d %s: got %v", c.status, c.code, got)
		}
	}
}

func TestProviderRefusalDoesNotEchoConversationOrProviderDiagnostics(t *testing.T) {
	secret := "sensitive-synthetic-input"
	f := providerFailure(hostModelResponse{StatusCode: 400, Body: []byte(`{"error":{"code":"context_length_exceeded","message":"sensitive-synthetic-input"}}`)})
	if !isOverflow(f) || f.Message == secret {
		t.Fatal(f)
	}
	f = providerFailure(hostModelResponse{StatusCode: 429, Body: []byte(`{"error":{"message":"sensitive-synthetic-input"}}`)})
	if f.HTTPStatus != 429 || isOverflow(f) || f.Message == secret {
		t.Fatal(f)
	}
}

func TestModelRefusalsDoNotRetryOrSaveAFold(t *testing.T) {
	for _, tc := range []struct {
		phase         string
		format        protocol
		stream, nudge bool
		method        string
	}{
		{"messages", claude, false, false, hostModelExecute},
		{"messages-stream", claude, true, false, hostModelExecuteStream},
		{"stored-summary", responses, true, true, hostModelExecute},
	} {
		for _, status := range []int{429, 502} {
			t.Run(tc.phase+"/"+strconv.Itoa(status), func(t *testing.T) {
				var methods []string
				plug := newPlugin(func(method string, _ any, into any) *rpcError {
					methods = append(methods, method)
					*into.(*hostModelResponse) = hostModelResponse{StatusCode: status, Body: []byte(`{"error":{"message":"synthetic diagnostic"}}`)}
					return nil
				})
				t.Cleanup(plug.shutdown)
				calls := map[string]int{}
				plug.startEngine = func(context.Context, context.Context) (engine, error) {
					return lifecycleWorker(t, func(op string, request map[string]json.RawMessage) (any, string) {
						calls[op]++
						switch op {
						case "prepare":
							return map[string]any{"body": request["body"], "foldRecommended": tc.nudge}, ""
						case "overflow":
							return map[string]any{"segments": []overflowSection{{"m00001", "m00003", "synthetic old history"}}}, ""
						default:
							return nil, ""
						}
					}), nil
				}
				body, _ := json.Marshal(map[string]any{"model": "fixture", "stream": tc.stream})
				turn := kernelTurn{p: plug, req: executorRequest{Model: "fixture", Format: tc.format, SourceFormat: tc.format, Stream: tc.stream}, session: strings.Repeat("a", 64), original: body}
				_, failure := turn.run(context.Background(), nil)
				if failure == nil || failure.HTTPStatus != status || !strings.Contains(failure.Message, strconv.Itoa(status)) || strings.Contains(failure.Message, "synthetic diagnostic") {
					t.Fatalf("status %d: %+v", status, failure)
				}
				if len(methods) != 1 || methods[0] != tc.method || calls["tool"] != 0 || calls["cache"] != 1 || (calls["overflow"] > 0) != tc.nudge {
					t.Fatalf("unexpected retries or fold: methods=%v kernel=%v", methods, calls)
				}
			})
		}
	}
}
