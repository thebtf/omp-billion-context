package main

import "testing"

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
