package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const pluginID = "omp-billion-context"

//go:embed VERSION
var pluginVersion string

type protocol string

const (
	chat      protocol = "openai"
	responses protocol = "openai-response"
)

var paths = map[protocol]string{
	chat:      "/v1/chat/completions",
	responses: "/v1/responses",
}

type routeRequest struct {
	RequestedModel string
	SourceFormat   protocol
	Headers        http.Header
	Query          url.Values
	Metadata       map[string]any
	Body           []byte
}

type routeResponse struct {
	Handled    bool
	TargetKind string `json:",omitempty"`
}

type executorRequest struct {
	Model           string
	SourceFormat    protocol
	Format          protocol
	Alt             string
	Stream          bool
	Headers         http.Header
	Query           url.Values
	Metadata        map[string]any
	OriginalRequest []byte
	Payload         []byte
	StreamID        string `json:"stream_id"`
	HostCallbackID  string `json:"host_callback_id"`
}

type executorResponse struct {
	Payload []byte
	Headers http.Header
}

type streamResponse struct {
	Headers http.Header `json:"headers"`
}

type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type rpcEnvelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *rpcError `json:"error,omitempty"`
}

type hostCall func(method string, payload any, into any) *rpcError

type engineAttempt struct {
	done   chan struct{}
	engine engine
	relay  *modelRelay
	err    error
}

type plugin struct {
	engineMu     sync.Mutex
	attempt      *engineAttempt
	startEngine  func(startupCtx, lifetimeCtx context.Context) (engine, error)
	shutdownOnce sync.Once
	call         hostCall
	ctx          context.Context
	cancel       context.CancelFunc
	streamMu     sync.Mutex
	streams      sync.WaitGroup
	streamCount  int
	streamIdle   chan struct{}
	skips        skipReport
}

func main() {}

func newPlugin(call hostCall) *plugin {
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &plugin{call: call, ctx: ctx, cancel: cancel, startEngine: startEngine, streamIdle: idle}
}

func (p *plugin) runningEngine(ctx context.Context) (engine, *modelRelay, error) {
	if err := ctx.Err(); err != nil {
		return engine{}, nil, err
	}
	p.engineMu.Lock()
	if err := p.ctx.Err(); err != nil {
		p.engineMu.Unlock()
		return engine{}, nil, err
	}
	if p.attempt == nil {
		p.attempt = &engineAttempt{done: make(chan struct{})}
		go p.runEngineAttempt(p.attempt)
	}
	attempt := p.attempt
	p.engineMu.Unlock()

	select {
	case <-ctx.Done():
		return engine{}, nil, ctx.Err()
	case <-p.ctx.Done():
		return engine{}, nil, p.ctx.Err()
	case <-attempt.done:
	}
	if err := ctx.Err(); err != nil {
		return engine{}, nil, err
	}
	if err := p.ctx.Err(); err != nil {
		return engine{}, nil, err
	}
	if attempt.err != nil {
		return engine{}, nil, attempt.err
	}
	select {
	case <-attempt.engine.done:
		return engine{}, nil, errEngineExited
	default:
		return attempt.engine, attempt.relay, nil
	}
}

func (p *plugin) runEngineAttempt(attempt *engineAttempt) {
	ctx, cancel := context.WithTimeout(p.ctx, engineStartupTimeout)
	defer cancel()
	defer close(attempt.done)
	if err := ctx.Err(); err != nil {
		attempt.err = err
		return
	}
	relay, err := newModelRelay()
	if err != nil {
		attempt.err = &engineStartupError{stage: "relay", cause: err}
		return
	}
	started, err := p.startEngine(ctx, p.ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		started.close()
		relay.stop()
		attempt.err = err
		return
	}
	attempt.engine, attempt.relay = started, relay
}

func (p *plugin) shutdown() {
	p.shutdownOnce.Do(func() {
		p.streamMu.Lock()
		p.cancel()
		p.streamMu.Unlock()
		p.streams.Wait()
		p.engineMu.Lock()
		attempt := p.attempt
		p.engineMu.Unlock()
		if attempt != nil {
			<-attempt.done
			attempt.engine.close()
			if attempt.relay != nil {
				attempt.relay.stop()
			}
		}
	})
}

func (p *plugin) handle(method string, raw []byte) (any, *rpcError) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registration(), nil
	case "management.register":
		return registerManagement(), nil
	case "management.handle":
		return p.handleManagement(raw), nil
	case "plugin.shutdown":
		p.shutdown()
		return struct{}{}, nil
	case "model.route":
		var req routeRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, &rpcError{Code: "invalid_request", Message: "invalid model route request"}
		}
		reason := alternateSkipReason(req.Query)
		if reason == compressible {
			reason, _ = classify(req.RequestedModel, req.SourceFormat, req.Headers, req.Metadata, req.Body)
		}
		if reason != compressible {
			p.reportSkip(reason, req.RequestedModel)
			return routeResponse{}, nil
		}
		return routeResponse{Handled: true, TargetKind: "self"}, nil
	case "executor.identifier":
		return map[string]string{"identifier": pluginID}, nil
	case "executor.execute", "executor.execute_stream":
		var req executorRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, &rpcError{Code: "invalid_request", Message: "invalid executor request", HTTPStatus: http.StatusBadRequest}
		}
		stream := method == "executor.execute_stream"
		if p.call == nil || req.HostCallbackID == "" || stream != req.Stream || (stream && req.StreamID == "") {
			return nil, &rpcError{Code: "invalid_request", Message: "invalid executor context", HTTPStatus: http.StatusBadRequest}
		}
		return p.execute(req)
	default:
		return nil, &rpcError{Code: "unsupported_method", Message: "unsupported plugin method", HTTPStatus: http.StatusNotImplemented}
	}
}

func registration() any {
	formats := []protocol{chat, responses}
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]string{
			"Name": pluginID, "Version": strings.TrimSpace(pluginVersion), "Author": "thebtf",
			"GitHubRepository": "https://github.com/thebtf/omp-billion-context",
		},
		"capabilities": map[string]any{
			"model_router": true, "executor": true, "executor_model_scope": "static", "management_api": true,
			"executor_input_formats": formats, "executor_output_formats": formats,
		},
	}
}
