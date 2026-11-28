package main

import (
	"bytes"
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
