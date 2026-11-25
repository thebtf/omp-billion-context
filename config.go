package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

func requestSession(model string, format protocol, headers http.Header, metadata map[string]any, payload []byte) string {
	path, supported := paths[format]
	scope, _ := metadata["caller_scope"].(string)
	requestPath, _ := metadata["request_path"].(string)
	source, _ := metadata["source"].(string)
	if model == "" || !supported || scope == "" || requestPath != path ||
		source == "plugin_host_model_callback" || headers.Get("Upgrade") != "" {
		return ""
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
		return ""
	}
	if format == chat && len(body.Messages) == 0 {
		return ""
	}
	if format == responses {
		if len(body.Input) == 0 || body.PreviousResponseID != "" || (len(body.Conversation) != 0 && string(body.Conversation) != "null") || body.Background {
			return ""
		}
		if body.Input[0] == '[' {
			var items []struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(body.Input, &items) != nil || len(items) == 0 {
				return ""
			}
			for _, item := range items {
				switch item.Type {
				case "compaction", "compaction_trigger", "item_reference":
					return ""
				}
				if item.EncryptedContent != "" {
					return ""
				}
			}
		} else if body.Input[0] != '"' {
			return ""
		}
	}
	var session string
	for _, key := range []string{"X-Acp-Session", "X-Session-Id", "Session-Id", "Session_Id", "X-Claude-Code-Session-Id", "X-Opencode-Session-Id"} {
		for _, value := range headers.Values(key) {
			if !validSession(value) || (session != "" && value != session) {
				return ""
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
	if !validSession(session) {
		return ""
	}
	digest := sha256.Sum256([]byte(scope + "\x00" + string(format) + "\x00" + model + "\x00" + session))
	return hex.EncodeToString(digest[:])
}

func validSession(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.ContainsAny(value, " ,\r\n\t\x00") {
		return false
	}
	return true
}
