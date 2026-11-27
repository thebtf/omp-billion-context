package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// requestSession returns the internal conversation identity, or "" when the
// request must stay on the native CPA path. classify owns the eligibility rules
// so routing and execution cannot drift apart.
func requestSession(model string, format protocol, headers http.Header, metadata map[string]any, payload []byte) string {
	reason, session := classify(model, format, headers, metadata, payload)
	if reason != compressible {
		return ""
	}
	scope, _ := metadata["caller_scope"].(string)
	digest := sha256.Sum256([]byte(scope + "\x00" + string(format) + "\x00" + model + "\x00" + session))
	return hex.EncodeToString(digest[:])
}

func validSession(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.ContainsAny(value, " ,\r\n\t\x00") {
		return false
	}
	return true
}
