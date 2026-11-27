package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

const maxSSEFrameBytes = 8 << 20

type streamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
}

type streamClose struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func (p *plugin) forwardStream(req executorRequest, body io.Reader) {
	closeMessage := streamClose{StreamID: req.StreamID}
	defer func() { p.call("host.stream.close", closeMessage, nil) }()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32<<10), maxSSEFrameBytes)
	var frame sseFrame
	// handle processes one complete SSE frame and reports whether the stream
	// must stop. A server may close right after the last data line without the
	// terminating blank line, so the same path runs once more at EOF.
	var finished, complete bool
	handle := func() {
		if len(frame.data) == 0 {
			frame.reset()
			return
		}
		if req.Format == chat && bytes.Equal(frame.data, []byte("[DONE]")) {
			// CPA writes the Chat Completions terminal sentinel, so reading
			// must stop here even if the upstream keeps the body open.
			finished, complete = true, true
			return
		}
		if !json.Valid(frame.data) {
			closeMessage.Error = "invalid billion-context stream event"
			finished = true
			return
		}
		if bytes.ContainsRune(frame.data, '\n') {
			var compact bytes.Buffer
			_ = json.Compact(&compact, frame.data)
			frame.data = append(frame.data[:0], compact.Bytes()...)
		}
		payload, terminal := frame.data, false
		switch req.Format {
		case responses:
			var kind struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(frame.data, &kind)
			switch kind.Type {
			case "response.completed", "response.incomplete", "response.failed", "error":
				terminal = true
			}
			payload = frame.responsesPayload()
		case claude:
			var kind struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(frame.data, &kind)
			// Anthropic closes a stream with message_stop; an error event ends
			// it too, because nothing else follows.
			switch kind.Type {
			case "message_stop", "error":
				terminal = true
			}
			payload = frame.eventPayload(kind.Type)
		}
		if failure := p.call("host.stream.emit", streamEmit{StreamID: req.StreamID, Payload: payload}, nil); failure != nil {
			closeMessage.Error = "CPA could not deliver a billion-context stream event"
			finished = true
			return
		}
		if terminal {
			closeMessage.Error = ""
			finished, complete = true, true
			return
		}
		frame.reset()
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) != 0 {
			if len(frame.data)+len(line) > maxSSEFrameBytes {
				closeMessage.Error = "oversized billion-context stream event"
				return
			}
			frame.add(line)
			continue
		}
		handle()
		if finished {
			return
		}
	}
	handle()
	if finished {
		return
	}
	if !complete {
		closeMessage.Error = "billion-context stream ended before its terminal event"
	}
	if scanner.Err() != nil {
		closeMessage.Error = "billion-context stream read failed"
	}
}

type sseFrame struct {
	event string
	data  []byte
}

func (f *sseFrame) add(line []byte) {
	name, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(name) {
	case "event":
		f.event = string(value)
	case "data":
		if len(f.data) != 0 {
			f.data = append(f.data, '\n')
		}
		f.data = append(f.data, value...)
	}
}

func (f *sseFrame) reset() {
	f.event, f.data = "", f.data[:0]
}

// eventPayload frames one SSE event the way Anthropic streams are read: an
// event line plus one data line. The type is taken from the body when bili did
// not send an explicit event line, because CPA routes on that name.
func (f *sseFrame) eventPayload(kind string) []byte {
	name := f.event
	if name == "" {
		name = kind
	}
	payload := make([]byte, 0, len(name)+len(f.data)+16)
	if name != "" {
		payload = append(payload, "event: "...)
		payload = append(payload, name...)
		payload = append(payload, '\n')
	}
	for data := f.data; ; {
		line, next, more := bytes.Cut(data, []byte("\n"))
		payload = append(payload, "data: "...)
		payload = append(payload, line...)
		payload = append(payload, '\n')
		if !more {
			break
		}
		data = next
	}
	return append(payload, '\n')
}

func (f *sseFrame) responsesPayload() []byte {
	payload := make([]byte, 0, len(f.event)+len(f.data)+16)
	if f.event != "" {
		payload = append(payload, "event: "...)
		payload = append(payload, f.event...)
		payload = append(payload, '\n')
	}
	for data := f.data; ; {
		line, next, more := bytes.Cut(data, []byte("\n"))
		payload = append(payload, "data: "...)
		payload = append(payload, line...)
		payload = append(payload, '\n')
		if !more {
			break
		}
		data = next
	}
	return append(payload, '\n')
}
