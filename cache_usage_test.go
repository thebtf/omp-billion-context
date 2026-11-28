package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProviderCacheUsagePreservesUnknownAndExplicitZero(t *testing.T) {
	var u wireUsage
	u.record([]byte(`{"usage":{"prompt_tokens":900}}`))
	if s := u.sample(chat); s.Read != nil || *s.Input != 900 {
		t.Fatal(s)
	}
	u.record([]byte(`{"usage":{"prompt_tokens_details":{"cached_tokens":0}}}`))
	if s := u.sample(chat); s.Read == nil || *s.Read != 0 || s.Write != nil {
		t.Fatal(s)
	}
}

func TestAnthropicCacheDenominatorIncludesCreationAndRead(t *testing.T) {
	var u wireUsage
	u.record([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":75,\"cache_creation_input_tokens\":5}}}\n\n"))
	u.record([]byte(`{"type":"message_delta","usage":{"output_tokens":90}}`))
	if s := u.sample(claude); s.Input == nil || *s.Input != 100 || *s.Read != 75 || *s.Write != 5 {
		t.Fatal(s)
	}
}

func TestResponsesCacheUsesFinalCumulativeUsage(t *testing.T) {
	var u wireUsage
	u.record([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":250,"input_tokens_details":{"cached_tokens":128}}}}`))
	u.record([]byte(`{"response":{"usage":{"input_tokens":250,"input_tokens_details":{"cached_tokens":128}}}}`))
	if s := u.sample(responses); s.Input == nil || *s.Input != 250 || *s.Read != 128 {
		t.Fatal(s)
	}
}

func TestResponsesCacheWriteAbsentZeroAndMeasured(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   int64
	}{{`{}`, -1}, {`{"cache_write_tokens":0}`, 0}, {`{"cache_write_tokens":64}`, 64}} {
		var u wireUsage
		u.record([]byte(`{"usage":{"input_tokens":100,"input_tokens_details":` + tc.detail + `}}`))
		got := u.sample(responses).Write
		if tc.want < 0 {
			if got != nil {
				t.Fatal(*got)
			}
		} else if got == nil || *got != tc.want {
			t.Fatal(tc.detail, got)
		}
	}
}

func TestRejectedStreamOpenRecordsOneCacheObservation(t *testing.T) {
	for _, tc := range []struct {
		status   int
		id, body string
		zero     bool
	}{
		{http.StatusTooManyRequests, "", `{}`, false}, {http.StatusBadGateway, "unused", `{"usage":{"prompt_tokens":4,"prompt_tokens_details":{"cached_tokens":0}}}`, true}, {http.StatusOK, "", `{}`, false},
	} {
		var observations []providerCacheUsage
		p := newPlugin(nil)
		p.startEngine = func(context.Context, context.Context) (engine, error) {
			return lifecycleWorker(t, func(op string, req map[string]json.RawMessage) (any, string) {
				if op == "cache" {
					var sample providerCacheUsage
					if err := json.Unmarshal(req["usage"], &sample); err != nil {
						return nil, err.Error()
					}
					observations = append(observations, sample)
				}
				return nil, ""
			}), nil
		}
		t.Cleanup(p.shutdown)
		observe := p.cacheObserved(context.Background(), strings.Repeat("a", 64), chat, "turn", func(method string, _ any, into any) *rpcError {
			if method == hostModelExecuteStream {
				*into.(*hostModelResponse) = hostModelResponse{StatusCode: tc.status, StreamID: tc.id, Body: []byte(tc.body)}
			}
			return nil
		})
		var reply hostModelResponse
		observe(hostModelExecuteStream, nil, &reply)
		if len(observations) != 1 {
			t.Fatalf("status %d: no immediate observation", tc.status)
		}
		read := observations[0].Read
		if (read != nil) != tc.zero || (read != nil && *read != 0) {
			t.Fatal("unknown and zero confused", read)
		}
		observe(hostModelStreamClose, nil, nil)
		if len(observations) != 1 {
			t.Fatal("double-counted request")
		}
	}
}
