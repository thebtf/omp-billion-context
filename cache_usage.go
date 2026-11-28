package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
)

type providerCacheUsage struct {
	Read  *int64 `json:"readTokens,omitempty"`
	Write *int64 `json:"writeTokens,omitempty"`
	Input *int64 `json:"inputTokens,omitempty"`
}

type wireUsage struct {
	Input         *int64 `json:"input_tokens"`
	Prompt        *int64 `json:"prompt_tokens"`
	Read          *int64 `json:"cache_read_input_tokens"`
	Write         *int64 `json:"cache_creation_input_tokens"`
	PromptDetails struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		Cached *int64 `json:"cached_tokens"`
		Write  *int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

func nonnegative(n *int64) *int64 {
	if n != nil && *n >= 0 {
		return n
	}
	return nil
}

func (u *wireUsage) merge(next *wireUsage) {
	if next == nil {
		return
	}
	for _, field := range []struct {
		to   **int64
		from *int64
	}{
		{&u.Input, next.Input}, {&u.Prompt, next.Prompt}, {&u.Read, next.Read}, {&u.Write, next.Write},
		{&u.PromptDetails.Cached, next.PromptDetails.Cached}, {&u.InputDetails.Cached, next.InputDetails.Cached}, {&u.InputDetails.Write, next.InputDetails.Write},
	} {
		if n := nonnegative(field.from); n != nil {
			*field.to = n
		}
	}
}

func (u wireUsage) sample(format protocol) providerCacheUsage {
	switch format {
	case chat:
		return providerCacheUsage{Read: u.PromptDetails.Cached, Input: u.Prompt}
	case responses:
		return providerCacheUsage{Read: u.InputDetails.Cached, Write: u.InputDetails.Write, Input: u.Input}
	case claude:
		s := providerCacheUsage{Read: u.Read, Write: u.Write}
		// Anthropic's input_tokens excludes cache reads and cache writes. Do not use a partial denominator.
		if u.Input != nil && u.Read != nil && u.Write != nil {
			total := *u.Input + *u.Read + *u.Write
			if total >= *u.Input && total >= *u.Read && total >= *u.Write {
				s.Input = &total
			}
		}
		return s
	}
	return providerCacheUsage{}
}

func (u *wireUsage) record(data []byte) {
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	parse := func(data []byte) {
		var packet struct {
			Usage    *wireUsage `json:"usage"`
			Response struct {
				Usage *wireUsage `json:"usage"`
			} `json:"response"`
			Message struct {
				Usage *wireUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &packet) == nil {
			u.merge(packet.Usage)
			u.merge(packet.Response.Usage)
			u.merge(packet.Message.Usage)
		}
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		parse(trimmed)
		return
	}
	var frame sseFrame
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if len(frame.data) > 0 {
				parse(frame.data)
				frame.reset()
			}
			continue
		}
		frame.add(line)
	}
	if len(frame.data) > 0 {
		parse(frame.data)
	}
}

func (p *plugin) cacheObserved(ctx context.Context, session string, format protocol, phase string, base hostCall) hostCall {
	var mu sync.Mutex
	var usage wireUsage
	var once sync.Once
	commit := func() {
		once.Do(func() {
			mu.Lock()
			sample := usage.sample(format)
			mu.Unlock()
			_ = p.kernelCall(ctx, "cache", map[string]any{"session": session, "phase": phase, "usage": sample}, nil)
		})
	}
	return func(method string, payload, into any) *rpcError {
		failure := base(method, payload, into)
		switch method {
		case hostModelExecute:
			if response, ok := into.(*hostModelResponse); ok && failure == nil {
				mu.Lock()
				usage.record(response.Body)
				mu.Unlock()
			}
			commit()
		case hostModelExecuteStream:
			if failure != nil {
				commit()
			}
		case hostModelStreamRead:
			if chunk, ok := into.(*hostModelStreamChunk); ok {
				mu.Lock()
				usage.record(chunk.Payload)
				mu.Unlock()
				if chunk.Done || chunk.Error != "" || failure != nil {
					commit()
				}
			}
		case hostModelStreamClose:
			commit()
		}
		return failure
	}
}
