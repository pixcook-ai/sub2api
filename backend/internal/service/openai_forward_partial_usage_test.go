package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
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
