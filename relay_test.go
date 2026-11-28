package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Feed the same raw SSE fixtures through the current host.stream_read contract.
func (p *plugin) forwardStream(req executorRequest, body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32768), maxSSEFrameBytes)
	read := func(method string, _ any, into any) *rpcError {
		if method == hostModelStreamClose {
			return nil
		}
		if method != hostModelStreamRead {
			panic(method)
		}
		var packet bytes.Buffer
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" && packet.Len() > 0 {
				break
			}
			packet.WriteString(line)
			packet.WriteByte('\n')
		}
		*into.(*hostModelStreamChunk) = hostModelStreamChunk{Payload: packet.Bytes(), Done: packet.Len() == 0}
		if err := scanner.Err(); err != nil {
			return kernelError("read_error", err.Error(), 502)
		}
		return nil
	}
	_, failure := readKernelStream(context.Background(), req.Format, nil, read, "fixture", func(payload []byte) error {
		f := p.call("host.stream.emit", streamEmit{req.StreamID, payload}, nil)
		if f != nil {
			return errors.New(f.Message)
		}
		return nil
	})
	end := streamClose{StreamID: req.StreamID}
	if failure != nil {
		end.Error = failure.Message
	}
	p.call("host.stream.close", end, nil)
}

func TestNativeContinuationPinsCallerModelAndHostCallback(t *testing.T) {
	turn := kernelTurn{req: executorRequest{Model: "existing-router-model", Format: chat, SourceFormat: chat, HostCallbackID: "turn-a", Headers: http.Header{"X-Session-Id": {"same-conversation"}}, Metadata: map[string]any{"caller_scope": "fixture-a", "request_path": paths[chat]}}}
	for _, body := range []string{`{"model":"different"}`, `{"model":"existing-router-model","prompt_cache_key":"p"}`} {
		b, stream, err := bindModelBody([]byte(body), turn.req.Model)
		if err != nil {
			t.Fatal(err)
		}
		req := turn.request(b, stream)
		if req.Model != turn.req.Model || req.CallerScope != "fixture-a" || req.HostCallbackID != "turn-a" || req.Path != paths[chat] || !bytes.Contains(req.Body, []byte(`"existing-router-model"`)) {
			t.Fatalf("wrong callback binding: %+v", req)
		}
	}
}

func TestDirectStreamCancellationClosesBlockedHostRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := make(chan struct{})
	closed := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	call := func(method string, _ any, into any) *rpcError {
		switch method {
		case hostModelStreamRead:
			once.Do(func() { close(read) })
			<-closed
			*into.(*hostModelStreamChunk) = hostModelStreamChunk{Done: true}
		case hostModelStreamClose:
			close(closed)
		}
		return nil
	}
	go func() {
		defer close(done)
		_, _ = readKernelStream(ctx, chat, nil, call, "fixture", func([]byte) error { return nil })
	}()
	<-read
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation left the provider read blocked")
	}
}

func TestClientToolNamedCompressIsPreserved(t *testing.T) {
	answer := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"client","function":{"name":"compress","arguments":"{}"}}]}}]}`)
	r, got, err := kernelJSON(chat, answer, map[string]bool{"compress": true}, "")
	if err != nil || len(r.Calls) != 0 || !r.External || !strings.Contains(string(got), "client") {
		t.Fatal(r, string(got), err)
	}
}
