package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// bili answers a refused preflight summarization with a structured error
// instead of a model reply, and marks the streams it commits before that
// summarization finishes. Those are the only things this file reads.
const (
	preflightCode       = "preflight_compress_failed"
	preflightRetryCode  = "compression_retry"
	preflightFailedCode = "compression_failed"
	// After its hold grace bili commits 200 with the final content type and
	// then reports the outcome in-band. This header is how that stream says so;
	// a stream without it was never held and must not be read ahead.
	preflightHoldHeader = "X-Bili-Preflight"
	preflightHolding    = "compressing"
	// bili's pinned diagnosis for a summary model that answers without visible
	// text. It is the only verdict-less refusal that is evidence of a settled
	// failure: the Anthropic and Responses error events carry no verdict, and
	// absence alone proves nothing.
	preflightDeadSummary = "consecutive completions ended normally (end_turn/stop/completed) with zero answer text"
	// A refusal is a small JSON object; a body beyond this is not one we parse.
	maxPreflightBytes = 64 << 10
)

var preflightMarker = []byte(preflightCode)

// preflightRefusal is bili's verdict on a failed summarization. Retryable is nil
// when the frame did not carry it; settled then carries the one diagnosis bili
// states in words instead of a field.
type preflightRefusal struct {
	Retryable *bool
	settled   bool
}

// parsePreflightRefusal accepts every shape bili uses for the refusal: a body or
// chat frame with a top-level error object, a Responses error event with a
// top-level code, and an Anthropic error event with a nested one.
func parsePreflightRefusal(payload []byte) (preflightRefusal, bool) {
	// bili writes a refusal as the whole, small body of the turn, so a payload
	// beyond the refusal budget is an answer. That keeps a 64 MiB success from
	// being scanned for the marker at all.
	if len(payload) > maxPreflightBytes {
		return preflightRefusal{}, false
	}
	// The code is a fixed token, so one pass finds it without decoding an
	// ordinary answer or a text delta.
	if !bytes.Contains(payload, preflightMarker) {
		return preflightRefusal{}, false
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return preflightRefusal{}, false
	}
	refusal := preflightRefusal{}
	var message string
	switch {
	case envelope.Error != nil && envelope.Error.Code == preflightCode:
		refusal.Retryable, message = envelope.Error.Retryable, envelope.Error.Message
	case envelope.Code == preflightCode:
		message = envelope.Message
	default:
		return preflightRefusal{}, false
	}
	refusal.settled = strings.Contains(message, preflightDeadSummary)
	return refusal, true
}

// readPreflightRefusal reads a non-stream body up to the refusal budget. The
// parser rejects anything larger, so a long success is never parsed as a frame.
func readPreflightRefusal(body io.Reader) (preflightRefusal, bool) {
	limited, err := io.ReadAll(io.LimitReader(body, maxPreflightBytes+1))
	if err != nil {
		return preflightRefusal{}, false
	}
	return parsePreflightRefusal(limited)
}

// peekHeldStream reads the comment keep-alives bili writes while it holds a
// committed stream and stops at the first data frame, which is where a refusal
// arrives. The returned reader still carries every byte it read, so a stream
// that turns out to be an ordinary answer is forwarded unchanged. Peak memory is
// one line buffer: a longer line is not a refusal and is replayed.
func peekHeldStream(body io.Reader) (io.Reader, preflightRefusal, bool) {
	reader := bufio.NewReaderSize(body, 64<<10)
	var consumed []byte
	var frame sseFrame
	for {
		line, err := reader.ReadSlice('\n')
		consumed = append(consumed, line...)
		switch {
		case err == bufio.ErrBufferFull || len(consumed) > maxSSEFrameBytes:
			return io.MultiReader(bytes.NewReader(consumed), reader), preflightRefusal{}, false
		case len(bytes.TrimRight(line, "\r\n")) != 0:
			frame.add(bytes.TrimRight(line, "\r\n"))
		case len(frame.data) != 0:
			if refusal, ok := parsePreflightRefusal(frame.data); ok {
				return nil, refusal, true
			}
			return io.MultiReader(bytes.NewReader(consumed), reader), preflightRefusal{}, false
		}
		if err != nil {
			return io.MultiReader(bytes.NewReader(consumed), reader), preflightRefusal{}, false
		}
	}
}

func (r preflightRefusal) permanent() bool {
	if r.Retryable != nil {
		return !*r.Retryable
	}
	return r.settled
}

// message is deliberately independent of bili's text: that text is derived from
// the provider exchange and can quote prompt spans and upstream error bodies.
func (r preflightRefusal) message() string {
	if !r.permanent() {
		return "compression is incomplete; retrying this message continues it"
	}
	if r.settled {
		return "the summary model answers without visible text, so this conversation cannot be compressed"
	}
	return "compression could not produce a visible summary for this conversation"
}

// failure keeps a 4xx refusal as upstream described it and reports a permanent
// 5xx as 422, so a client does not repeat work bili already proved futile. An
// undecided or retryable verdict stays temporary: bili stores the applied folds
// in the session, so the next attempt starts from a smaller history.
func (r preflightRefusal) failure(status int) *rpcError {
	if r.permanent() {
		if status < 400 || status > 499 {
			status = http.StatusUnprocessableEntity
		}
		return &rpcError{Code: preflightFailedCode, Message: r.message(), HTTPStatus: status}
	}
	// A rate limit or an unavailable upstream is its own temporary verdict, and
	// the client should back off on it rather than on a generic gateway fault.
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return &rpcError{Code: preflightRetryCode, Message: r.message(), HTTPStatus: status}
	}
	return &rpcError{Code: preflightRetryCode, Message: r.message(), HTTPStatus: http.StatusServiceUnavailable}
}

// scrubPreflightFrame rebuilds one in-band refusal in the same frame shape bili
// uses, keeping only the fields bili itself emits. A version that adds the
// upstream body, a detail or a provider-derived parameter cannot pass it on.
func scrubPreflightFrame(payload []byte, refusal preflightRefusal) []byte {
	var frame struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return payload
	}
	message := refusal.message()
	switch {
	case frame.Error != nil && frame.Error.Code == preflightCode:
		nested := map[string]any{"type": "server_error", "code": preflightCode, "message": message}
		if refusal.Retryable != nil {
			// A verdict the frame really carried is kept; one it lacked is never
			// added, because the Anthropic and Responses shapes have no place for it.
			nested["retryable"] = *refusal.Retryable
		}
		envelope := map[string]any{"error": nested}
		if frame.Type != "" {
			// Anthropic routes on the outer type and nests the stage, exactly as
			// bili emits it.
			envelope["type"] = "error"
		}
		scrubbed, err := json.Marshal(envelope)
		if err != nil {
			return payload
		}
		return scrubbed
	case frame.Code == preflightCode:
		// Responses error event: type, code and text, plus a real verdict.
		out := map[string]any{"type": "error", "code": preflightCode, "message": message}
		if refusal.Retryable != nil {
			out["retryable"] = *refusal.Retryable
		}
		scrubbed, err := json.Marshal(out)
		if err != nil {
			return payload
		}
		return scrubbed
	}
	return payload
}
