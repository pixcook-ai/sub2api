package middleware

import (
	"net/http"
	"testing"
)

func TestStoredResponseManagementRequestMatchesOnlyResourceMethodsAndOneID(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		want               bool
	}{
		{"retrieve local response", http.MethodGet, "/v1/responses/resp_local_0123456789abcdef0123456789abcdef", true},
		{"delete local response", http.MethodDelete, "/v1/responses/resp_local_0123456789abcdef0123456789abcdef", true},
		{"unknown ID reaches resource not found", http.MethodGet, "/v1/responses/unknown", true},
		{"bare responses stays websocket", http.MethodGet, "/v1/responses", false},
		{"empty ID", http.MethodGet, "/v1/responses/", false},
		{"post remains inference", http.MethodPost, "/v1/responses/response-id", false},
		{"patch unsupported", http.MethodPatch, "/v1/responses/response-id", false},
		{"head unsupported", http.MethodHead, "/v1/responses/response-id", false},
		{"input items unsupported", http.MethodGet, "/v1/responses/response-id/input_items", false},
		{"cancel unsupported", http.MethodDelete, "/v1/responses/response-id/cancel", false},
		{"trailing slash unsupported", http.MethodGet, "/v1/responses/response-id/", false},
		{"two ID segments unsupported", http.MethodGet, "/v1/responses/one/two", false},
		{"empty first segment unsupported", http.MethodGet, "/v1/responses//response-id", false},
		{"unregistered root alias", http.MethodGet, "/responses/response-id", false},
		{"different prefix", http.MethodGet, "/v1/responses-other/response-id", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStoredResponseManagementRequest(tc.method, tc.path); got != tc.want {
				t.Fatalf("isStoredResponseManagementRequest(%q, %q) = %t; want %t", tc.method, tc.path, got, tc.want)
			}
		})
	}
}
