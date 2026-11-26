package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
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

func (p *plugin) execute(req executorRequest) (any, *rpcError) {
	path := paths[req.SourceFormat]
	original := req.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	session := requestSession(req.Model, req.SourceFormat, req.Headers, req.Metadata, original)
	if path == "" || req.SourceFormat != req.Format || req.Alt != "" || session == "" || !json.Valid(req.Payload) {
		return nil, &rpcError{Code: "unsupported_request", Message: "request cannot be compressed safely", HTTPStatus: http.StatusBadRequest}
	}
	timeout, contentType := nonStreamTimeout, "application/json"
	if req.Stream {
		timeout, contentType = streamTimeout, "text/event-stream"
	}
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	running, relay, err := p.runningEngine(ctx)
	if err != nil {
		cancel()
		return nil, engineFailure(err)
	}
	origin, release, err := relay.begin(ctx, req, p.call)
	if err != nil {
		cancel()
		return nil, sidecarFailure(err)
	}
	finish := func() { cancel(); release() }
	detached := false
	defer func() {
		if !detached {
			finish()
		}
	}()
	destination := running.url
	destination.Path = "/bili/" + origin + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.String(), bytes.NewReader(req.Payload))
	if err != nil {
		return nil, sidecarFailure(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", contentType)
	request.Header.Set("X-Acp-Session", session)
	request.Header.Set("User-Agent", pluginID)
	request.Header.Set("X-Omp-Bili-Access", running.token)
	response, err := running.client.Do(request)
	if err != nil {
		return nil, sidecarFailure(err)
	}
	defer func() {
		if !detached {
			_ = response.Body.Close()
		}
	}()
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || mediaType != contentType {
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		return nil, &rpcError{Code: "sidecar_response", Message: "billion-context could not complete the CPA model request", HTTPStatus: status}
	}
	headers := http.Header{"Content-Type": {contentType}}
	if req.Stream {
		p.streamMu.Lock()
		if p.ctx.Err() != nil {
			p.streamMu.Unlock()
			return nil, sidecarFailure(context.Canceled)
		}
		p.streams.Add(1)
		if p.streamCount == 0 {
			p.streamIdle = make(chan struct{})
		}
		p.streamCount++
		p.streamMu.Unlock()
		detached = true
		go func() {
			defer func() {
				p.streamMu.Lock()
				p.streamCount--
				if p.streamCount == 0 {
					close(p.streamIdle)
				}
				p.streamMu.Unlock()
				p.streams.Done()
			}()
			defer finish()
			defer response.Body.Close()
			p.forwardStream(req, response.Body)
		}()
		return streamResponse{Headers: headers}, nil
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, sidecarFailure(err)
	}
	if len(result) > maxResponseBytes || !json.Valid(result) {
		return nil, &rpcError{Code: "sidecar_response", Message: "billion-context returned an invalid or oversized JSON response", HTTPStatus: http.StatusBadGateway}
	}
	return executorResponse{Payload: result, Headers: headers}, nil
}

func engineFailure(err error) *rpcError {
	failure := &rpcError{Code: "engine_unavailable", Message: "bundled billion-context is unavailable", HTTPStatus: http.StatusServiceUnavailable}
	var startup *engineStartupError
	if errors.As(err, &startup) {
		failure.Message = startup.Error()
	} else if errors.Is(err, errEngineExited) {
		failure.Message = errEngineExited.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		failure.Code = "engine_timeout"
		failure.HTTPStatus = http.StatusGatewayTimeout
		if startup == nil {
			failure.Message = "bundled billion-context startup timed out"
		}
	}
	return failure
}

func sidecarFailure(err error) *rpcError {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return &rpcError{Code: "sidecar_timeout", Message: "billion-context request timed out", HTTPStatus: http.StatusGatewayTimeout}
	}
	return &rpcError{Code: "sidecar_failure", Message: "billion-context request failed", HTTPStatus: http.StatusBadGateway}
}
