package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Exercise the existing Forward boundary with the trusted context produced by
// managed-file admission. Actual admission/ownership and database settlement
// remain separate HTTP/PG acceptance gates.
func TestOpenAIForwardManagedBPSFailurePreservesTerminalAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"input":"hello"}`, stream))
			status, payload := http.StatusTooManyRequests, bpsTestFailure
			if stream {
				status = http.StatusOK
				payload = "event: response.failed\ndata: " + `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","output":[],"error":{"status":429,"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached on tokens per minute (TPM). Please try again later."},"usage":{"input_tokens":9,"output_tokens":4}}}` + "\n\n"
			}
			response := newOpenAIRejectedFieldTestResponse(status, payload)
			response.Header.Set("X-Bps-Response-Contract", "failure-v1")
			if stream {
				response.Header.Set("Content-Type", "text/event-stream")
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			a := managedSchedulerTestAccount(5107, 8)
			account := &a
			authorizations := 0
			ctx := withManagedFileGeneration(context.Background(), account.ID, "private-service-key", "admission", "owner", "group", "binding", func(context.Context) error {
				authorizations++
				return nil
			})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = newOpenAIRejectedFieldTestContext(body).Request.WithContext(ctx)
			c.Request.Header.Set("X-Bps-File-Admission", "client-forgery")
			result, err := svc.Forward(ctx, c, account, body)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.NotNil(t, result)
			require.Equal(t, "resp_failed", result.ResponseID)
			require.Equal(t, 9, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			require.Equal(t, status, recorder.Code)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, 1, authorizations)
			require.Equal(t, "admission", upstream.lastReq.Header.Get("X-Bps-File-Admission"))
			require.Contains(t, recorder.Body.String(), `"status":"failed"`)
			require.NotContains(t, recorder.Body.String(), "private-service-key")
			require.NotContains(t, recorder.Body.String(), "client-forgery")
			if stream {
				require.Contains(t, recorder.Body.String(), "event: response.failed\n")
			}
			// A second invocation using the same admitted attempt must stop before
			// another HTTP send, even though the first terminal was an error.
			_, replayErr := svc.Forward(ctx, c, account, body)
			require.Error(t, replayErr)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, 1, authorizations)
		})
	}
}
