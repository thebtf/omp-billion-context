package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

const (
	hostModelExecute       = "host.model.execute"
	hostModelExecuteStream = "host.model.execute_stream"
	hostModelStreamRead    = "host.model.stream_read"
	hostModelStreamClose   = "host.model.stream_close"
)

type hostModelRequest struct {
	EntryProtocol  protocol    `json:"entry_protocol"`
	ExitProtocol   protocol    `json:"exit_protocol"`
	Model          string      `json:"model"`
	Stream         bool        `json:"stream"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	Query          url.Values  `json:"query"`
	Alt            string      `json:"alt"`
	Path           string      `json:"path,omitempty"`
	CallerScope    string      `json:"caller_scope,omitempty"`
	HostCallbackID string      `json:"host_callback_id"`
}

type hostModelResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
	StreamID   string      `json:"stream_id"`
}

type hostModelStreamRequest struct {
	StreamID string `json:"stream_id"`
}

type hostModelStreamChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func bindModelBody(body []byte, model string) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, false, errors.New("model payload must be a JSON object")
	}
	var stream bool
	if raw, ok := fields["stream"]; ok && json.Unmarshal(raw, &stream) != nil {
		return nil, false, errors.New("model payload stream must be a boolean")
	}
	var bodyModel string
	if json.Unmarshal(fields["model"], &bodyModel) == nil && bodyModel == model {
		return body, stream, nil
	}
	fields["model"], _ = json.Marshal(model)
	bound, err := json.Marshal(fields)
	return bound, stream, err
}

func modelStatus(status int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return http.StatusBadGateway
}
