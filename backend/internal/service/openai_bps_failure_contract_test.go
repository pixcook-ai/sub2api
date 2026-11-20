package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const bpsTestFailure = `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later."},"response":{"id":"resp_failed","status":"failed","usage":{"input_tokens":9,"output_tokens":4}}}`

func TestBPSFailureContractValidatesSelectedResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		body    string
		status  int
		oauth   bool
		valid   bool
	}{
		{name: "valid", headers: []string{"failure-v1"}, body: bpsTestFailure, status: 429, valid: true},
		{name: "missing", body: bpsTestFailure, status: 429},
		{name: "version", headers: []string{"failure-v2"}, body: bpsTestFailure, status: 429},
		{name: "duplicates", headers: []string{"failure-v1", "failure-v1"}, body: bpsTestFailure, status: 429},
		{name: "comma", headers: []string{"failure-v1,failure-v1"}, body: bpsTestFailure, status: 429},
		{name: "case", headers: []string{"Failure-v1"}, body: bpsTestFailure, status: 429},
		{name: "status mismatch", headers: []string{"failure-v1"}, body: bpsTestFailure, status: 502},
		{name: "OAuth", headers: []string{"failure-v1"}, body: bpsTestFailure, status: 429, oauth: true},
		{name: "private message", headers: []string{"failure-v1"}, body: strings.Replace(bpsTestFailure, "Rate limit reached on tokens per minute (TPM). Please try again later.", "private prompt", 1), status: 429},
		{name: "duplicate escaped key", headers: []string{"failure-v1"}, body: strings.Replace(bpsTestFailure, `"status":"failed"`, `"status":"failed","sta\u0074us":"completed"`, 1), status: 429},
		{name: "unknown envelope", headers: []string{"failure-v1"}, body: strings.TrimSuffix(bpsTestFailure, "}") + `,"private":"value"}`, status: 429},
		{name: "trailing document", headers: []string{"failure-v1"}, body: bpsTestFailure + `{}`, status: 429},
		{name: "successful diagnostic", headers: []string{"failure-v1"}, body: strings.Replace(bpsTestFailure, `"status":"failed"`, `"status":"completed"`, 1), status: 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			if tc.oauth {
				account.Type = AccountTypeOAuth
			}
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"X-Bps-Response-Contract": tc.headers}}
			_, valid := bpsNonStreamingFailure(account, response, []byte(tc.body))
			require.Equal(t, tc.valid, valid)
		})
	}
}

func TestBPSFailureSafeUsageCounts(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"input_tokens":-1}`, `{"input_tokens":"9"}`, `{"input_tokens":1.5}`, `{"input_tokens":9007199254740992}`, `{"input_tokens":1e999}`, `{"input_tokens":1e999999999}`, `{"input_tokens":1e-999999999}`, `{"input_tokens":9007199254740991.4}`} {
		require.Nil(t, safeOpenAIFailureUsage(gjson.Parse(raw)), raw)
	}
	safe := safeOpenAIFailureUsage(gjson.Parse(`{"input_tokens":0,"output_tokens":4,"private":"secret","input_tokens_details":{"cached_tokens":2,"private":"secret"}}`))
	require.NotNil(t, safe)
	require.NotContains(t, safe, "private")
}

func TestOpenAIForwardBPSFailureUnknownUsageDoesNotSettle(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)
	for _, usage := range []string{`null`, `{}`, `{"total_tokens":13}`, `{"input_tokens":-1}`} {
		response := newOpenAIRejectedFieldTestResponse(429, strings.Replace(bpsTestFailure, `{"input_tokens":9,"output_tokens":4}`, usage, 1))
		response.Header.Set("X-Bps-Response-Contract", "failure-v1")
		upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = newOpenAIRejectedFieldTestContext(body).Request
		result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, 429, recorder.Code)
		require.Len(t, upstream.bodies, 1)
	}
}

func TestOpenAIForwardBPSFailureInboundMarkerCannotPreventFailover(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(429, bpsTestFailure)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.Header.Set("X-Bps-Response-Contract", "failure-v1")
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Nil(t, result)
}

func TestBPSFailureRealClassifierShapes(t *testing.T) {
	for _, tc := range []struct {
		code, kind, message, event string
		status                     int
	}{
		{"rate_limit_exceeded", "tokens", "Rate limit reached on tokens per minute (TPM). Please try again later.", "failed", 429},
		{"basispoints_upstream_cancelled", "server_error", "Basispoints cancelled the response; request was not replayed.", "cancelled", 502},
		{"overloaded_error", "server_error", "Basispoints upstream request failed.", "failed", 503},
		{"message_too_big", "invalid_request_error", "Basispoints upstream request failed.", "failed", 413},
		{"invalid_value", "rate_limit_error", "Rate limit reached. Please try again later.", "failed", 429},
		{"basispoints_model_access_changed", "permission_error", "Basispoints denied access for this account.", "failed", 403},
		{"basispoints_protocol_error", "server_error", "Basispoints returned an invalid response stream.", "failed", 502},
	} {
		t.Run(tc.code, func(t *testing.T) {
			errorObject := map[string]any{"status": tc.status, "type": tc.kind, "code": tc.code, "message": tc.message}
			if tc.code == "basispoints_protocol_error" {
				delete(errorObject, "status")
				errorObject["failure_stage"] = "bridge_protocol_conversion"
			}
			diagnostic := map[string]any{"id": "resp_real", "status": tc.event, "output": []any{}, "error": errorObject, "usage": map[string]any{"input_tokens": 9, "output_tokens": 4}}
			raw, err := json.Marshal(map[string]any{"status": tc.status, "type": "response." + tc.event, "response": diagnostic})
			require.NoError(t, err)
			response := &http.Response{StatusCode: 200, Header: http.Header{"X-Bps-Response-Contract": []string{"failure-v1"}}}
			require.True(t, bpsStreamingFailure(newOpenAIRejectedFieldTestAccount(), response, raw))
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			response.Body = io.NopCloser(strings.NewReader("data: " + string(raw) + "\n\n"))
			response.Header.Set("Content-Type", "text/event-stream")
			upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = newOpenAIRejectedFieldTestContext(body).Request
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.NotNil(t, result)
			require.Equal(t, 9, result.Usage.InputTokens)
			require.Equal(t, 200, recorder.Code)
			require.Len(t, upstream.bodies, 1)
			delete(errorObject, "status")
			delete(errorObject, "failure_stage")
			delete(diagnostic, "output")
			delete(diagnostic, "error")
			jsonBody, err := json.Marshal(map[string]any{"error": errorObject, "response": diagnostic})
			require.NoError(t, err)
			response.StatusCode = tc.status
			_, valid := bpsNonStreamingFailure(newOpenAIRejectedFieldTestAccount(), response, jsonBody)
			require.True(t, valid)
		})
	}
}

func TestOpenAIForwardBPSFailureMixedUsageSettlesOnlySafeCounters(t *testing.T) {
	for _, invalid := range []string{"9007199254740992", "1.5", "9007199254740991.4"} {
		for _, progress := range []bool{false, true} {
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			prefix := ""
			if progress {
				prefix = "data: " + `{"type":"response.in_progress","response":{"usage":{"input_tokens":7}}}` + "\n\n"
			}
			terminal := `{"type":"response.failed","response":{"id":"resp_mixed","status":"failed","error":{"status":429,"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later."},"usage":{"input_tokens":` + invalid + `,"output_tokens":4}}}`
			response := newOpenAIRejectedFieldTestResponse(200, prefix+"data: "+terminal+"\n\n")
			response.Header.Set("Content-Type", "text/event-stream")
			response.Header.Set("X-Bps-Response-Contract", "failure-v1")
			upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = newOpenAIRejectedFieldTestContext(body).Request
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
			require.Error(t, err)
			require.NotNil(t, result)
			wantInput := 0
			if progress {
				wantInput = 7
			}
			require.Equal(t, wantInput, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			require.NotContains(t, recorder.Body.String(), invalid)
			require.Contains(t, recorder.Body.String(), `"output_tokens":4`)
		}
	}
}

func TestOpenAIForwardBPSMarkedSuccessKeepsNativeUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(200, "data: "+`{"type":"response.completed","response":{"id":"resp_success","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4,"input_tokens_details":{"image_tokens":3},"output_tokens_details":{"image_tokens":2}}}}`+"\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Set("X-Bps-Response-Contract", "failure-v1")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 3, result.Usage.ImageInputTokens)
	require.Equal(t, 2, result.Usage.ImageOutputTokens)
}

func TestOpenAIForwardBPSBareErrorRetainsProgressUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	failure := `{"status":429,"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later."}`
	terminal := `{"type":"error","status":429,"code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later.","error":` + failure + `,"response":{"id":"resp_progress","status":"failed","output":[],"error":` + failure + `,"usage":{"input_tokens":7}}}`
	payload := "data: " + `{"type":"response.in_progress","response":{"id":"resp_progress","usage":{"input_tokens":7}}}` + "\n\nevent: error\ndata: " + terminal + "\n\n"
	response := newOpenAIRejectedFieldTestResponse(200, payload)
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Set("X-Bps-Response-Contract", "failure-v1")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = newOpenAIRejectedFieldTestContext(body).Request
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.NotErrorAs(t, err, &failover)
	require.NotNil(t, result)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 200, recorder.Code)
	require.Len(t, upstream.bodies, 1)
	require.Contains(t, recorder.Body.String(), `"type":"response.failed"`)
	eventName := ""
	sawFailure := false
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") && gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "type").String() == "response.failed" {
			require.Equal(t, "response.failed", eventName)
			sawFailure = true
		}
		if line == "" {
			eventName = ""
		}
	}
	require.True(t, sawFailure)
}

func TestBPSBareErrorRejectsUnknownOrContradictoryEnvelope(t *testing.T) {
	response := &http.Response{StatusCode: 200, Header: http.Header{"X-Bps-Response-Contract": []string{"failure-v1"}}}
	errorJSON := `{"status":429,"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later."}`
	valid := `{"type":"error","status":429,"error":` + errorJSON + `}`
	for _, raw := range []string{
		strings.Replace(valid, `"status":429`, `"status":502`, 1),
		strings.Replace(valid, `"type":"error"`, `"type":"error","private":"secret"`, 1),
		strings.Replace(valid, `"type":"error"`, `"type":"error","code":"other"`, 1),
		strings.Replace(valid, "Rate limit reached on tokens per minute (TPM). Please try again later.", "private", 1),
	} {
		_, ok := normalizeBPSBareError(newOpenAIRejectedFieldTestAccount(), response, []byte(raw))
		require.False(t, ok, raw)
	}
}
