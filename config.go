package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// requestSession returns the identity the engine is addressed with, and whether
// the request may be compressed at all. An empty identity is legitimate: bili
// derives a content fingerprint for a client that sent none, so the plugin must
// not invent one that would merge separate conversations.
func requestSession(model string, format protocol, headers http.Header, metadata map[string]any, payload []byte) (string, bool) {
	reason, session := classify(model, format, headers, metadata, payload)
	if reason != compressible {
		return "", false
	}
	if session == "" {
		return "", true
	}
	scope, _ := metadata["caller_scope"].(string)
	digest := sha256.Sum256([]byte(scope + "\x00" + string(format) + "\x00" + model + "\x00" + session))
	return hex.EncodeToString(digest[:]), true
}

func validSession(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.ContainsAny(value, " ,\r\n\t\x00") {
		return false
	}
	return true
}
