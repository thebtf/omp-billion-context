package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	nonStreamTimeout = 2 * time.Minute
	streamTimeout    = 15 * time.Minute
	maxResponseBytes = 64 << 20
)

func (p *plugin) execute(req executorRequest) (any, *rpcError) {
	original := req.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	session, groups := requestSession(req.Model, req.SourceFormat, req.Headers, req.Metadata, original)
	if session == "" || paths[req.SourceFormat] == "" || req.SourceFormat != req.Format || req.Alt != "" || !json.Valid(req.Payload) {
		return nil, kernelError("unsupported_request", "Request cannot be compressed safely", 400)
	}
	if req.SourceFormat == chat && !bytes.Equal(original, req.Payload) {
		reason, _, _ := classify(req.Model, chat, req.Headers, req.Metadata, req.Payload)
		if reason != compressible {
			return nil, kernelError("unsupported_request", "Incompatible Chat execution payload", 400)
		}
	}
	if req.SourceFormat == responses && !bytes.Equal(original, req.Payload) {
		reason, _, actual := classify(req.Model, responses, req.Headers, req.Metadata, req.Payload)
		if reason != compressible || len(actual) != len(groups) || (len(groups) > 0 && !groups.intact(req.Payload)) {
			return nil, kernelError("unsupported_request", "Incompatible Responses execution payload", 400)
		}
	}
	timeout := nonStreamTimeout
	if req.Stream {
		timeout = streamTimeout
	}
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	unlock, err := p.turns.lock(ctx, session)
	if err != nil {
		cancel()
		return nil, sidecarFailure(err)
	}
	finish := func() { unlock(); cancel() }
	turn := kernelTurn{p: p, req: req, session: session, groups: groups, client: toolNames(req.Payload), original: req.Payload}
	if !req.Stream {
		defer finish()
		payload, f := turn.run(ctx, nil)
		if f != nil {
			return nil, f
		}
		return executorResponse{Payload: payload, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
	}
	p.streamMu.Lock()
	if p.ctx.Err() != nil {
		p.streamMu.Unlock()
		finish()
		return nil, sidecarFailure(context.Canceled)
	}
	p.streams.Add(1)
	if p.streamCount == 0 {
		p.streamIdle = make(chan struct{})
	}
	p.streamCount++
	p.streamMu.Unlock()
	ready := make(chan *rpcError, 1)
	go func() {
		defer finish()
		defer func() {
			p.streamMu.Lock()
			p.streamCount--
			if p.streamCount == 0 {
				close(p.streamIdle)
			}
			p.streamMu.Unlock()
			p.streams.Done()
		}()
		started := false
		_, f := turn.run(ctx, func(chunk []byte) error {
			if !started {
				started = true
				ready <- nil
			}
			if fail := p.call("host.stream.emit", streamEmit{StreamID: req.StreamID, Payload: chunk}, nil); fail != nil {
				return errors.New("CPA could not deliver stream event")
			}
			return nil
		})
		if !started && f != nil {
			ready <- f
			return
		}
		if !started {
			ready <- nil
		}
		closeMsg := streamClose{StreamID: req.StreamID}
		if f != nil {
			closeMsg.Error = f.Message
		}
		p.call("host.stream.close", closeMsg, nil)
	}()
	select {
	case f := <-ready:
		if f != nil {
			return nil, f
		}
		return streamResponse{Headers: http.Header{"Content-Type": {"text/event-stream"}}}, nil
	case <-ctx.Done():
		return nil, sidecarFailure(ctx.Err())
	}
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
