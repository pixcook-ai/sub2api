package service

import (
	"encoding/json"
	"net/http"
	"strings"
)

// isBPSRequestPolicyRejection only suppresses request-mutating compatibility
// retries. The marker does not establish execution, billing or account health.
func isBPSRequestPolicyRejection(status int, headers http.Header, body []byte) bool {
	values := headers.Values("X-Bps-Request-Policy")
	if status != http.StatusBadRequest || len(values) != 1 || values[0] != "rejected-v1" {
		return false
	}
	var response struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil || response.Error.Type != "invalid_request_error" || strings.TrimSpace(response.Error.Param) == "" {
		return false
	}
	switch response.Error.Code {
	case "unsupported_parameter", "unsupported_tool", "unsupported_tool_choice", "invalid_parameter":
		return true
	default:
		return false
	}
}
