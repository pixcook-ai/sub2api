package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIForwardFailedStreamPreservesReportedUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(http.StatusOK,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+
			"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"code\":\"upstream_error\",\"message\":\"generation failed\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Set("x-request-id", "partial-usage-request")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	c := newOpenAIRejectedFieldTestContext(body)
	groupID := int64(81)
	c.Set("api_key", &APIKey{ID: 82, GroupID: &groupID})
	svc := newOpenAIRejectedFieldTestService(upstream)

	result, err := svc.Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)

	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.NotErrorAs(t, err, &failover)
	require.NotNil(t, result, "reported usage must reach the existing error-settlement path")
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, "partial-usage-request", result.RequestID)
	require.Equal(t, "gpt-5.5", result.Model)
	require.True(t, result.Stream)
	require.Len(t, upstream.bodies, 1)
	bound, bindErr := svc.getOpenAIWSStateStore().GetResponseAccount(context.Background(), groupID, "resp_failed")
	require.NoError(t, bindErr)
	require.Zero(t, bound, "a failed response must not become a reusable successful binding")
}

func TestOpenAIForwardFailedStreamDistinguishesUnknownAndReportedZeroUsage(t *testing.T) {
	for _, test := range []struct {
		name, usage string
		wantResult  bool
	}{
		{name: "missing"},
		{name: "null", usage: `,"usage":null`},
		{name: "empty object", usage: `,"usage":{}`},
		{name: "unrelated object", usage: `,"usage":{"total_tokens":13}`},
		{name: "string count", usage: `,"usage":{"input_tokens":"9"}`},
		{name: "negative count", usage: `,"usage":{"input_tokens":-9}`},
		{name: "reported zero", usage: `,"usage":{"input_tokens":0,"output_tokens":0}`, wantResult: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			response := newOpenAIRejectedFieldTestResponse(http.StatusOK,
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+
					fmt.Sprintf("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"upstream_error\",\"message\":\"generation failed\"}%s}}\n\n", test.usage))
			response.Header.Set("Content-Type", "text/event-stream")
			upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
			require.Error(t, err)
			if test.wantResult {
				require.NotNil(t, result)
				require.Zero(t, result.Usage.InputTokens)
				require.Zero(t, result.Usage.OutputTokens)
			} else {
				require.Nil(t, result, "unknown usage must not become a fabricated zero settlement")
			}
		})
	}
}

func TestOpenAIForwardTruncatedStreamPreservesProgressiveUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(http.StatusOK,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
	require.ErrorContains(t, err, "missing terminal event")
	require.NotNil(t, result)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Len(t, upstream.bodies, 1)
}

func TestOpenAIForwardFailoverStreamDoesNotPublishPartialSettlement(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(http.StatusOK,
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"account_disabled\",\"message\":\"Your account is disabled\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Nil(t, result)
	require.Len(t, upstream.bodies, 1)
}

func TestOpenAIForwardFailedStreamDoesNotBorrowUsageEvidenceFromAnotherObject(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(http.StatusOK,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+
			"data: {\"type\":\"response.failed\",\"usage\":{},\"response\":{\"error\":{\"code\":\"upstream_error\",\"message\":\"generation failed\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
	require.Error(t, err)
	require.Nil(t, result, "the parser selected the empty top-level object, so nested counts cannot validate fabricated zero")
}

func TestOpenAIForwardFailedStreamPreservesSafeClientTerminal(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	response := newOpenAIRejectedFieldTestResponse(http.StatusOK,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+
			"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"instructions\":\"private instruction\",\"output\":[{\"type\":\"private output\"}],\"error\":{\"code\":\"upstream_error\",\"message\":\"generation failed\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13,\"private_field\":\"private usage\",\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = newOpenAIRejectedFieldTestContext(body).Request
	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
	require.Error(t, err)
	var terminal string
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") && gjson.Get(strings.TrimPrefix(line, "data: "), "type").String() == "response.failed" {
			terminal = strings.TrimPrefix(line, "data: ")
		}
	}
	require.NotEmpty(t, terminal)
	require.Equal(t, int64(9), gjson.Get(terminal, "response.usage.input_tokens").Int())
	require.Equal(t, int64(4), gjson.Get(terminal, "response.usage.output_tokens").Int())
	require.Equal(t, int64(13), gjson.Get(terminal, "response.usage.total_tokens").Int())
	require.Equal(t, int64(2), gjson.Get(terminal, "response.usage.input_tokens_details.cached_tokens").Int())
	require.Equal(t, int64(1), gjson.Get(terminal, "response.usage.output_tokens_details.reasoning_tokens").Int())
	require.Equal(t, "[]", gjson.Get(terminal, "response.output").Raw)
	require.NotContains(t, terminal, "private")
}

func TestOpenAIForwardBPSMarkedFailureRetainsProtocol(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"input":"hello"}`, stream))
			diagnostic := `{"id":"resp_failed","status":"failed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}`
			failure := `{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later."}`
			payload := `{"error":` + failure + `,"response":` + diagnostic + `}`
			status := http.StatusTooManyRequests
			if stream {
				status = http.StatusOK
				payload = "data: " + `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","output":[],"error":` + strings.Replace(failure, "{", `{"status":429,`, 1) + `,"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}` + "\n\n"
			}
			response := newOpenAIRejectedFieldTestResponse(status, payload)
			response.Header.Set("X-Bps-Response-Contract", "failure-v1")
			if stream {
				response.Header.Set("Content-Type", "text/event-stream")
			}
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
			require.Equal(t, 4, result.Usage.OutputTokens)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, status, recorder.Code)
			require.Contains(t, recorder.Body.String(), `"input_tokens":9`)
			require.Contains(t, recorder.Body.String(), `"rate_limit_exceeded"`)
		})
	}
}
