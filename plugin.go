package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

const pluginID = "omp-billion-context"

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
	Metadata       struct {
		RequestPath string `json:"request_path"`
	}
}

type routeResponse struct {
	Handled    bool
	TargetKind string `json:",omitempty"`
}

type executorRequest struct {
	Model        string
	SourceFormat protocol
	Format       protocol
	Alt          string
	Stream       bool
	Headers      http.Header
	Payload      []byte
	StreamID     string `json:"stream_id"`
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

type hostCall func(method string, payload any) bool

type plugin struct {
	cfg      config
	engineMu sync.Mutex
	engine   engine
	call     hostCall
	ctx      context.Context
	cancel   context.CancelFunc
	streamMu sync.Mutex
	streams  sync.WaitGroup
}

func main() {}

func newPlugin(cfg config, call hostCall) *plugin {
	ctx, cancel := context.WithCancel(context.Background())
	p := &plugin{cfg: cfg, call: call, ctx: ctx, cancel: cancel}
	if cfg.sidecar.Host != "" {
		p.engine = engine{url: cfg.sidecar, client: sidecarClient(cfg.sidecar.Host)}
	}
	return p
}

func (p *plugin) runningEngine() (engine, error) {
	p.engineMu.Lock()
	defer p.engineMu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return engine{}, err
	}
	if p.engine.client == nil {
		started, err := startEngine(p.ctx, p.cfg.upstream)
		if err != nil {
			return engine{}, err
		}
		p.engine = started
	}
	return p.engine, nil
}

func (p *plugin) shutdown() {
	p.streamMu.Lock()
	p.cancel()
	p.streamMu.Unlock()
	p.streams.Wait()
	p.engineMu.Lock()
	defer p.engineMu.Unlock()
	if p.engine.client != nil {
		p.engine.client.CloseIdleConnections()
	}
	if p.engine.stop != nil {
		p.engine.stop()
	}
	p.engine = engine{}
}

func (p *plugin) handle(method string, raw []byte) (any, *rpcError) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registration(), nil
	case "plugin.shutdown":
		p.shutdown()
		return struct{}{}, nil
	case "model.route":
		var req routeRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, &rpcError{Code: "invalid_request", Message: "invalid model route request"}
		}
		path, supported := paths[req.SourceFormat]
		if supported && p.cfg.alias != "" && req.RequestedModel == p.cfg.alias && req.Headers.Get("Upgrade") == "" &&
			(req.Metadata.RequestPath == "" || req.Metadata.RequestPath == path) {
			// A self route keeps the requested model in CPA.
			return routeResponse{Handled: true, TargetKind: "self"}, nil
		}
		return routeResponse{}, nil
	case "executor.identifier":
		return map[string]string{"identifier": pluginID}, nil
	case "executor.execute", "executor.execute_stream":
		var req executorRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, &rpcError{Code: "invalid_request", Message: "invalid executor request", HTTPStatus: http.StatusBadRequest}
		}
		stream := method == "executor.execute_stream"
		if stream != req.Stream || (stream && (req.StreamID == "" || p.call == nil)) {
			return nil, &rpcError{Code: "invalid_request", Message: "invalid executor stream mode", HTTPStatus: http.StatusBadRequest}
		}
		return p.execute(req)
	default:
		return nil, &rpcError{Code: "unsupported_method", Message: "unsupported plugin method", HTTPStatus: http.StatusNotImplemented}
	}
}

func registration() any {
	formats := []protocol{chat, responses}
	return map[string]any{
		"schema_version": 1,
		"metadata": map[string]string{
			"Name": pluginID, "Version": "0.1.0", "Author": "thebtf",
			"GitHubRepository": "https://github.com/thebtf/omp-billion-context",
		},
		"capabilities": map[string]any{
			"model_router": true, "executor": true, "executor_model_scope": "static",
			"executor_input_formats": formats, "executor_output_formats": formats,
		},
	}
}
