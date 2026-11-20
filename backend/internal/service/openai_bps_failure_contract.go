package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// This response-only marker describes the selected API-key upstream's bounded
// failure envelope. It is not authentication or proof of exactly-once execution.
func hasBPSFailureContract(account *Account, headers http.Header) bool {
	values := headers.Values("X-Bps-Response-Contract")
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey && len(values) == 1 && values[0] == "failure-v1"
}

// Decode without duplicate decoded keys: otherwise validation and later JSON
// consumers can disagree on the status, usage or error selected for settlement.
func decodeBPSFailureObject(raw []byte) (map[string]any, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 32 {
			return nil, errors.New("failure envelope nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return token, nil
		}
		if delimiter == '{' {
			object := map[string]any{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				if _, exists := object[key]; exists {
					return nil, errors.New("duplicate object key")
				}
				value, err := read(depth + 1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return nil, errors.New("invalid object end")
			}
			return object, nil
		}
		if delimiter == '[' {
			values := []any{}
			for decoder.More() {
				value, err := read(depth + 1)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return nil, errors.New("invalid array end")
			}
			return values, nil
		}
		return nil, errors.New("invalid container")
	}
	value, err := read(0)
	if err != nil {
		return nil, false
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

// Messages are the gateway's fixed public classifier output, never raw upstream
// diagnostics. Only these combinations can bypass ordinary error replay.
func bpsFailureErrorStatus(value map[string]any, fallback int) int {
	if len(value) < 3 || len(value) > 8 {
		return 0
	}
	for key, raw := range value {
		switch key {
		case "type", "code", "message", "status":
		case "failure_stage", "rejected_tool", "repair_skipped_reason", "upstream_terminal_event":
			text, ok := raw.(string)
			if !ok || len(text) > 256 || value["code"] != "basispoints_protocol_error" || value["type"] != "server_error" {
				return 0
			}
			if key == "failure_stage" && text != "bridge_protocol_conversion" && text != "bridge_tool_validation" {
				return 0
			}
		default:
			return 0
		}
	}
	kind, _ := value["type"].(string)
	code, _ := value["code"].(string)
	message, _ := value["message"].(string)
	// SSE Details carries semantic status; JSON encodes UpstreamError without
	// it, using the actual HTTP status. Both are emitted by the same classifier.
	status := fallback
	if raw, exists := value["status"]; exists {
		number, ok := raw.(json.Number)
		if !ok {
			return 0
		}
		parsed, err := number.Int64()
		if err != nil || parsed < 400 || parsed > 599 {
			return 0
		}
		status = int(parsed)
		if fallback != 0 && status != fallback {
			return 0
		}
	}
	if status != 0 {
		knownCode := false
		switch code {
		case "invalid_request", "invalid_request_error", "bad_request_error", "invalid_argument", "invalid_value", "unsupported_value", "invalid_prompt", "context_length_exceeded", "string_above_max_length", "previous_response_not_found", "thinking_signature_invalid", "invalid_encrypted_content", "cyber_policy", "content_policy_violation", "authentication_error", "invalid_api_key", "token_expired", "invalid_token", "unauthorized", "permission_error", "permission_denied", "basispoints_model_access_changed", "access_denied", "forbidden", "model_not_found", "model_not_found_error", "unsupported_model", "message_too_big", "rate_limit_error", "rate_limit_exceeded", "insufficient_quota", "quota_exceeded", "usage_limit_reached", "tokens", "requests", "server_error", "internal_error", "internal_server_error", "server_is_overloaded", "overloaded_error", "service_unavailable", "api_error", "basispoints_upstream_error", "basispoints_upstream_cancelled", "basispoints_protocol_error":
			knownCode = true
		}
		if !knownCode {
			return 0
		}
		// Explicit upstream status can change the classifier category while
		// retaining its known code. Match its fixed message, never raw text.
		if status == 401 && kind == "authentication_error" && message == "Basispoints authentication failed. Refresh the ChatGPT login credentials." {
			return status
		}
		if status == 403 && kind == "permission_error" && message == "Basispoints denied access for this account." {
			return status
		}
		if status == 429 && kind == "rate_limit_error" && message == "Rate limit reached. Please try again later." {
			return status
		}

		if status >= 500 && kind == "server_error" {
			if message == "Basispoints upstream request failed." || code == "basispoints_protocol_error" && message == "Basispoints returned an invalid response stream." || code == "basispoints_upstream_cancelled" && message == "Basispoints cancelled the response; request was not replayed." {
				return status
			}
		}
		if status != 401 && status != 403 && status != 429 && status < 500 && kind == "invalid_request_error" && message == "Basispoints upstream request failed." {
			return status
		}
		// The remaining specialized public messages retain their native status.
	}

	classified := func() int {
		switch kind {
		case "tokens":
			if (code == "rate_limit_exceeded" || code == "rate_limit_error") && message == "Rate limit reached on tokens per minute (TPM). Please try again later." {
				return 429
			}
		case "requests":
			if (code == "rate_limit_exceeded" || code == "rate_limit_error") && message == "Rate limit reached on requests per minute (RPM). Please try again later." {
				return 429
			}
		case "rate_limit_error":
			if (code == "rate_limit_exceeded" || code == "rate_limit_error") && message == "Rate limit reached. Please try again later." {
				return 429
			}
		case "insufficient_quota":
			if (code == "insufficient_quota" || code == "quota_exceeded") && message == "Basispoints account quota is exhausted." {
				return 429
			}
		case "server_error":
			if code == "basispoints_protocol_error" && message == "Basispoints returned an invalid response stream." {
				return 502
			}
			if (code == "basispoints_upstream_error" || code == "server_error" || code == "internal_error" || code == "server_is_overloaded") && message == "Basispoints upstream request failed." {
				return 502
			}
		case "authentication_error":
			switch code {
			case "invalid_api_key", "invalid_token", "token_expired", "authentication_error", "unauthorized":
				if message == "Basispoints authentication failed. Refresh the ChatGPT login credentials." {
					return 401
				}
			}
		case "permission_error":
			switch code {
			case "permission_denied", "permission_error", "access_denied", "forbidden":
				if message == "Basispoints denied access for this account." {
					return 403
				}
			}
		case "invalid_request_error":
			switch code {
			case "invalid_request_error":
				if message == "Basispoints rejected the request." {
					return 400
				}
			case "context_length_exceeded":
				if message == "Basispoints request exceeds the model context limit." {
					return 400
				}
			case "model_not_found", "unsupported_model":
				if message == "The requested model is unavailable for this Basispoints account." {
					return 400
				}
			}
		}
		return 0
	}()
	if status != 0 && classified != status {
		return 0
	}
	return classified
}

func bpsFailureResponse(value map[string]any) (map[string]any, bool) {
	status, _ := value["status"].(string)
	if status != "failed" && status != "cancelled" {
		return nil, false
	}
	result := map[string]any{"status": status}
	if id, exists := value["id"]; exists {
		text, ok := id.(string)
		if !ok || len(text) > 256 {
			return nil, false
		}
		result["id"] = text
	}
	if raw, exists := value["usage"]; exists {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, false
		}
		if usage := safeOpenAIFailureUsage(gjson.ParseBytes(encoded)); usage != nil {
			result["usage"] = usage
		}
	}
	return result, true
}

func bpsNonStreamingFailure(account *Account, resp *http.Response, raw []byte) ([]byte, bool) {
	if !hasBPSFailureContract(account, resp.Header) {
		return nil, false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok || len(object) != 2 {
		return nil, false
	}
	failure, ok := object["error"].(map[string]any)
	if !ok || bpsFailureErrorStatus(failure, resp.StatusCode) != resp.StatusCode {
		return nil, false
	}
	response, ok := object["response"].(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range response {
		if key != "id" && key != "status" && key != "usage" {
			return nil, false
		}
	}
	diagnostic, ok := bpsFailureResponse(response)
	if !ok {
		return nil, false
	}
	encoded, err := json.Marshal(map[string]any{"error": failure, "response": diagnostic})
	return encoded, err == nil
}

func bpsStreamingFailure(account *Account, resp *http.Response, raw []byte) bool {
	if resp.StatusCode != http.StatusOK || !hasBPSFailureContract(account, resp.Header) {
		return false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok || (object["type"] != "response.failed" && object["type"] != "response.cancelled") {
		return false
	}
	response, ok := object["response"].(map[string]any)
	if !ok {
		return false
	}
	failure, ok := response["error"].(map[string]any)
	if !ok || bpsFailureErrorStatus(failure, 0) == 0 {
		return false
	}
	if response["status"] != strings.TrimPrefix(object["type"].(string), "response.") {
		return false
	}
	_, ok = bpsFailureResponse(response)
	return ok
}

// The bridge also emits a classified bare error as its terminal. Normalize it
// before ordinary pre-output retry logic; do not bless arbitrary raw errors.
func normalizeBPSBareError(account *Account, resp *http.Response, raw []byte) ([]byte, bool) {
	if resp.StatusCode != 200 || !hasBPSFailureContract(account, resp.Header) {
		return nil, false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok || object["type"] != "error" {
		return nil, false
	}
	for key := range object {
		switch key {
		case "type", "status", "error", "code", "message", "response", "sequence_number":
		default:
			return nil, false
		}
	}
	failure, ok := object["error"].(map[string]any)
	if !ok {
		return nil, false
	}
	status := bpsFailureErrorStatus(failure, 0)
	if status == 0 {
		return nil, false
	}
	if outer, exists := object["status"]; exists {
		number, ok := outer.(json.Number)
		if !ok {
			return nil, false
		}
		value, err := number.Int64()
		if err != nil || int64(status) != value {
			return nil, false
		}
	}
	for _, key := range []string{"code", "message"} {
		if outer, exists := object[key]; exists && outer != failure[key] {
			return nil, false
		}
	}
	response := map[string]any{"status": "failed", "output": []any{}, "error": failure}
	if nested, exists := object["response"]; exists {
		fields, ok := nested.(map[string]any)
		if !ok {
			return nil, false
		}
		diagnostic, ok := bpsFailureResponse(fields)
		if !ok || diagnostic["status"] != "failed" {
			return nil, false
		}
		if nestedError, exists := fields["error"]; exists {
			encoded, err := json.Marshal(nestedError)
			if err != nil {
				return nil, false
			}
			outer, _ := json.Marshal(failure)
			if !bytes.Equal(encoded, outer) {
				return nil, false
			}
		}
		for key, value := range diagnostic {
			response[key] = value
		}
	}
	normalized := map[string]any{"type": "response.failed", "response": response}
	if sequence, exists := object["sequence_number"]; exists {
		normalized["sequence_number"] = sequence
	}
	encoded, err := json.Marshal(normalized)
	return encoded, err == nil
}
