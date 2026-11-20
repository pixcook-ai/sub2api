//go:build unit

package service

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

func TestManagedFileGenerationSendsOnceAndOverridesPrivateHeaders(t *testing.T) {
	var authorized atomic.Int32
	ctx := withManagedFileGeneration(context.Background(), 7, "server-key", "admission", "owner", "group", "binding", func(context.Context) error { authorized.Add(1); return nil })
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.test/v1/responses", nil)
	req.Header.Set(managedfiles.ServiceKeyHeader, "forged")
	req.Header.Set("Idempotency-Key", "public")
	prepareManagedFileGenerationRequest(req)
	require.Equal(t, "server-key", req.Header.Get(managedfiles.ServiceKeyHeader))
	require.Empty(t, req.Header.Get("Idempotency-Key"))
	require.Nil(t, req.GetBody)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimManagedFileGeneration(req.Context(), 7) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
	require.EqualValues(t, 1, authorized.Load())
}

func TestManagedFileGenerationRejectsWrongAccountAndCancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	guarded := withManagedFileGeneration(ctx, 7, "key", "admit", "owner", "group", "binding", func(context.Context) error { t.Fatal("must not authorize"); return nil })
	require.Error(t, claimManagedFileGeneration(guarded, 8))
	cancel()
	require.Error(t, claimManagedFileGeneration(context.WithoutCancel(guarded), 7))
}

func TestManagedFileGenerationStripsUntrustedPrivateHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.test/v1/responses", nil)
	for _, key := range []string{managedfiles.ServiceKeyHeader, managedfiles.AdmissionHeader, managedfiles.OwnerRefHeader, managedfiles.GroupRefHeader, managedfiles.BindingHeader} {
		req.Header.Set(key, "forged")
	}
	prepareManagedFileGenerationRequest(req)
	for _, key := range []string{managedfiles.ServiceKeyHeader, managedfiles.AdmissionHeader, managedfiles.OwnerRefHeader, managedfiles.GroupRefHeader, managedfiles.BindingHeader} {
		require.Empty(t, req.Header.Get(key))
	}
}

func TestOpenAIForwardManagedGenerationDoesNotRepairRejectedRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello","service_tier":"priority"}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(400, `{"error":{"message":"Unsupported parameter: service_tier","type":"invalid_request_error","param":"service_tier","code":"unsupported_parameter"}}`)}}
	svc := newOpenAIRejectedFieldTestService(upstream)
	a := managedSchedulerTestAccount(7, 8)
	c := newOpenAIRejectedFieldTestContext(body)
	ctx := withManagedFileGeneration(context.Background(), 7, "server-key", "admission", "owner", "group", "binding", func(context.Context) error { return nil })
	_, err := svc.Forward(ctx, c, &a, body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
}
