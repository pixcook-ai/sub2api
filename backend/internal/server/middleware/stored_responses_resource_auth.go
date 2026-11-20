package middleware

import (
	"net/http"
	"strings"
)

// Management routes share resource authorization without making the bare
// Responses endpoint, inference requests, or future subresources billing-free.
func isStoredResponseManagementRequest(method, path string) bool {
	if method != http.MethodGet && method != http.MethodDelete {
		return false
	}
	const prefix = "/v1/responses/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id := strings.TrimPrefix(path, prefix)
	return id != "" && !strings.Contains(id, "/")
}
