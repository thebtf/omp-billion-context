package main

import "testing"

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
