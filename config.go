package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// requestSession returns a scoped identity and any protected input groups.
// An empty identity means the request stays on the native route. The digest always folds in caller, model
// and protocol, so the shared engine can only match a request within its own
// caller even when the client named no conversation of its own.
func requestSession(model string, format protocol, headers http.Header, metadata map[string]any, payload []byte) (string, protectedGroups) {
	reason, session, groups := classify(model, format, headers, metadata, payload)
	if reason != compressible {
		return "", nil
	}
	scope, _ := metadata["caller_scope"].(string)
	digest := sha256.Sum256([]byte(scope + "\x00" + string(format) + "\x00" + model + "\x00" + session))
	return hex.EncodeToString(digest[:]), groups
}

func validSession(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.ContainsAny(value, " ,\r\n\t\x00") {
		return false
	}
	return true
}
