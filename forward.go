package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	nonStreamTimeout = 2 * time.Minute
	streamTimeout    = 15 * time.Minute
	maxResponseBytes = 64 << 20
)

func sidecarClient(address string) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, address)
			},
			DisableCompression:     true,
			IdleConnTimeout:        30 * time.Second,
			ResponseHeaderTimeout:  90 * time.Second,
			MaxIdleConnsPerHost:    8,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (cfg config) authorized(headers http.Header) bool {
	values := headers.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(cfg.clientSum[:], sum[:]) == 1
}

func sessionID(headers http.Header) string {
	var id string
	for _, name := range [...]string{"X-Acp-Session", "X-Session-Id", "Session-Id", "Session_Id"} {
		for _, value := range headers.Values(name) {
			if len(value) > 256 || !bearerToken(value) || (id != "" && id != value) {
				return ""
			}
			id = value
		}
	}
	return id
}

func (p *plugin) execute(req executorRequest) (any, *rpcError) {
	path, supported := paths[req.SourceFormat]
	if !supported || p.cfg.alias == "" || req.SourceFormat != req.Format || req.Model != p.cfg.alias || req.Alt != "" {
		return nil, &rpcError{Code: "unsupported_request", Message: "unsupported model or protocol", HTTPStatus: http.StatusBadRequest}
	}
	if !p.cfg.authorized(req.Headers) {
		return nil, &rpcError{Code: "unauthorized", Message: "this model requires its configured bearer token", HTTPStatus: http.StatusUnauthorized}
	}
	session := sessionID(req.Headers)
	if session == "" {
		return nil, &rpcError{Code: "invalid_session", Message: "supply one stable session ID using X-Acp-Session or an equivalent session header", HTTPStatus: http.StatusBadRequest}
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(req.Payload, &body) != nil || body == nil {
		return nil, &rpcError{Code: "invalid_payload", Message: "model payload must be a JSON object", HTTPStatus: http.StatusBadRequest}
	}
	body["model"], _ = json.Marshal(p.cfg.model)
	if req.Stream {
		body["stream"] = json.RawMessage("true")
	} else {
		body["stream"] = json.RawMessage("false")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &rpcError{Code: "invalid_payload", Message: "invalid model payload", HTTPStatus: http.StatusBadRequest}
	}

	// Keep the authority fixed. Bili routes the configured absolute upstream embedded in the path.
	running, err := p.runningEngine()
	if err != nil {
		return nil, &rpcError{Code: "engine_unavailable", Message: "could not start bundled billion-context", HTTPStatus: http.StatusServiceUnavailable}
	}
	destination := running.url
	destination.Path = "/bili/" + p.cfg.upstream + path
	timeout, contentType := nonStreamTimeout, "application/json"
	if req.Stream {
		timeout, contentType = streamTimeout, "text/event-stream"
	}
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.String(), bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, sidecarFailure(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", contentType)
	request.Header.Set("X-Acp-Session", session)
	request.Header.Set("User-Agent", pluginID)
	if p.cfg.upstreamToken != "" {
		request.Header.Set("Authorization", "Bearer "+p.cfg.upstreamToken)
	}
	if running.token != "" {
		request.Header.Set("X-Omp-Bili-Access", running.token)
	}
	response, err := running.client.Do(request)
	if err != nil {
		cancel()
		return nil, sidecarFailure(err)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || mediaType != contentType {
		response.Body.Close()
		cancel()
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		return nil, &rpcError{Code: "sidecar_response", Message: "billion-context returned an unsuccessful or unexpected response", HTTPStatus: status}
	}
	headers := http.Header{"Content-Type": {contentType}}
	if req.Stream {
		p.streamMu.Lock()
		if p.ctx.Err() != nil {
			p.streamMu.Unlock()
			response.Body.Close()
			cancel()
			return nil, sidecarFailure(context.Canceled)
		}
		p.streams.Add(1)
		p.streamMu.Unlock()
		go func() {
			defer p.streams.Done()
			defer cancel()
			defer response.Body.Close()
			p.forwardStream(req, response.Body)
		}()
		return streamResponse{Headers: headers}, nil
	}
	defer cancel()
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, sidecarFailure(err)
	}
	if len(result) > maxResponseBytes || !json.Valid(result) {
		return nil, &rpcError{Code: "sidecar_response", Message: "billion-context returned an invalid or oversized JSON response", HTTPStatus: http.StatusBadGateway}
	}
	return executorResponse{Payload: result, Headers: headers}, nil
}

func sidecarFailure(err error) *rpcError {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return &rpcError{Code: "sidecar_timeout", Message: "billion-context request timed out", HTTPStatus: http.StatusGatewayTimeout}
	}
	return &rpcError{Code: "sidecar_failure", Message: "billion-context request failed", HTTPStatus: http.StatusBadGateway}
}
