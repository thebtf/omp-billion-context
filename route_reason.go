package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// skipReason names why a request stayed on the native CPA path. "compressible"
// means the request is eligible; every other value is a deliberate bypass.
type skipReason string

const (
	compressible       skipReason = "compressible"
	otherEndpoint      skipReason = "other_endpoint"
	unsupportedFormat  skipReason = "unsupported_format"
	alternateMode      skipReason = "alternate_mode"
	missingCallerScope skipReason = "missing_caller_scope"
	nestedCallback     skipReason = "nested_callback"
	websocketUpgrade   skipReason = "websocket"
	invalidPayload     skipReason = "invalid_payload"
	emptyConversation  skipReason = "empty_conversation"
	opaqueContinuation skipReason = "opaque_continuation"
	encryptedReasoning skipReason = "encrypted_reasoning"
	missingSessionID   skipReason = "missing_session_id"
	ambiguousSessionID skipReason = "ambiguous_session_id"
	unusableSessionID  skipReason = "unusable_session_id"
)

// silentReasons are the bypasses an operator can actually fix. The rest are
// normal traffic that CPA sends to plugin model routers for every model request.
var silentReasons = map[skipReason]bool{
	missingCallerScope: true,
	missingSessionID:   true,
	ambiguousSessionID: true,
	unusableSessionID:  true,
}

var sessionHeaders = []string{
	"X-Acp-Session", "X-Session-Id", "Session-Id", "Session_Id",
	"X-Claude-Code-Session-Id", "X-Opencode-Session-Id",
	// Harnesses that speak the Anthropic API name their conversation
	// differently; without these a /v1/messages caller is never compressed.
	"Agent-Session-Id", "X-Conversation-Id", "X-Opencode-Session",
}

// classify is the single eligibility rule shared by routing and execution. It
// also returns the caller's conversation ID, so the payload is parsed once.
func classify(model string, format protocol, headers http.Header, metadata map[string]any, payload []byte) (skipReason, string) {
	path, supported := paths[format]
	scope, _ := metadata["caller_scope"].(string)
	requestPath, _ := metadata["request_path"].(string)
	source, _ := metadata["source"].(string)
	switch {
	case model == "" || !supported:
		return unsupportedFormat, ""
	case requestPath != path:
		return otherEndpoint, ""
	case source == "plugin_host_model_callback":
		return nestedCallback, ""
	case headers.Get("Upgrade") != "":
		return websocketUpgrade, ""
	case scope == "":
		return missingCallerScope, ""
	}
	var body struct {
		Messages           []json.RawMessage `json:"messages"`
		Input              json.RawMessage   `json:"input"`
		PromptCacheKey     string            `json:"prompt_cache_key"`
		SessionID          string            `json:"session_id"`
		PreviousResponseID string            `json:"previous_response_id"`
		Conversation       json.RawMessage   `json:"conversation"`
		Background         bool              `json:"background"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return invalidPayload, ""
	}
	if format == chat && len(body.Messages) == 0 {
		return emptyConversation, ""
	}
	// Anthropic calls carry the system prompt outside messages, so the same
	// "is there a conversation" test applies to a non-empty messages array.
	if format == claude && len(body.Messages) == 0 {
		return emptyConversation, ""
	}
	if format == responses {
		if len(body.Input) == 0 {
			return emptyConversation, ""
		}
		if body.PreviousResponseID != "" || (len(body.Conversation) != 0 && string(body.Conversation) != "null") || body.Background {
			return opaqueContinuation, ""
		}
		if body.Input[0] == '[' {
			var items []struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(body.Input, &items) != nil || len(items) == 0 {
				return invalidPayload, ""
			}
			for _, item := range items {
				switch item.Type {
				case "compaction", "compaction_trigger", "item_reference":
					return opaqueContinuation, ""
				}
				if item.EncryptedContent != "" {
					return encryptedReasoning, ""
				}
			}
		} else if body.Input[0] != '"' {
			return invalidPayload, ""
		}
	}
	// A malformed value is not a distinct failure: it simply carries no
	// identity, so it is skipped and the caller falls back to the next source.
	// Only a genuine conflict between usable values blocks compression.
	session := ""
	for _, key := range sessionHeaders {
		distinct := make(map[string]bool, 2)
		for _, value := range headers.Values(key) {
			if validSession(value) {
				distinct[value] = true
			}
		}
		if len(distinct) > 1 {
			return ambiguousSessionID, ""
		}
		for value := range distinct {
			if session != "" && value != session {
				return ambiguousSessionID, ""
			}
			session = value
		}
	}
	if session == "" {
		session = body.SessionID
	}
	if session == "" {
		session = body.PromptCacheKey
	}
	// No explicit id is not a reason to refuse, and an empty identity cannot be
	// sent as-is: the engine is shared by every CPA caller, and bili's anonymous
	// matcher has no caller boundary, so two callers with the same opening would
	// share compression state. The conversation's opening turn gives the identity
	// here, and requestSession folds caller, model and protocol into the value it
	// sends, so the engine can only ever match a request to its own caller.
	var opening []json.RawMessage
	if format == responses && len(body.Input) != 0 && body.Input[0] == '[' {
		_ = json.Unmarshal(body.Input, &opening)
	} else {
		opening = body.Messages
	}
	if session == "" {
		session = openingTurn(opening)
	}
	if !validSession(session) {
		return unusableSessionID, ""
	}
	return compressible, session
}

func alternateSkipReason(query map[string][]string) skipReason {
	alt := ""
	if values, ok := query["alt"]; ok && len(values) > 0 {
		alt = values[0]
	} else if values, ok := query["$alt"]; ok && len(values) > 0 {
		alt = values[0]
	}
	if alt == "" || alt == "sse" {
		return compressible
	}
	return alternateMode
}

// openingTurn returns a stable identity built from the first user turn. It is
// deliberately narrow: only the text of that one message takes part, so later
// growth of the conversation cannot change it. Responses carries the same shape
// under `input`, and a part there is `input_text` instead of `text`; both are
// read so an unidentified Responses conversation is not refused outright.
func openingTurn(messages []json.RawMessage) string {
	for _, raw := range messages {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &message) != nil {
			continue
		}
		if message.Role != "user" || len(message.Content) == 0 {
			continue
		}
		text := ""
		if message.Content[0] == '"' {
			var value string
			if json.Unmarshal(message.Content, &value) == nil {
				text = value
			}
		} else if message.Content[0] == '[' {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(message.Content, &parts) == nil {
				var builder strings.Builder
				for _, part := range parts {
					if part.Type == "text" || part.Type == "input_text" {
						builder.WriteString(part.Text)
					}
				}
				text = builder.String()
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		return "turn-" + hex.EncodeToString(digest[:16])
	}
	return ""
}

func describeSkip(reason skipReason) string {
	return strings.ReplaceAll(string(reason), "_", " ")
}
