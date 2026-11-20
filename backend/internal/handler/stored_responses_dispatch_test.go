//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type storedResponseDispatchRepo struct {
	*grokCredentialHandlerRepo
	listCalls int
}

func (r *storedResponseDispatchRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]service.Account, error) {
	r.listCalls++
	return r.accounts, nil
}

// The shared Codex fixture supplies a fake authenticated subject. This wrapper
// also supplies the explicit model authorization port required by local storage.
func newStoredResponsesDispatchEntryWithConfig(t *testing.T, accounts []service.Account, usage service.UsageLogRepository, configure func(*config.Config), schedulerSlots bool, repos ...service.AccountRepository) (*codexWireUpstream, *gin.Engine, func()) {
	t.Helper()
	return newStoredResponsesDispatchEntry(t, accounts, usage, configure, schedulerSlots, false, repos...)
}

func newStoredResponsesDispatchEntry(t *testing.T, accounts []service.Account, usage service.UsageLogRepository, configure func(*config.Config), schedulerSlots, authorize bool, repos ...service.AccountRepository) (*codexWireUpstream, *gin.Engine, func()) {
	t.Helper()
	var accountRepo service.AccountRepository = &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}
	if len(repos) > 0 {
		accountRepo = repos[0]
	}
	groups := &groupMapRepo{groups: map[int64]*service.Group{9001: {ID: 9001, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true}}}
	var snapshot *service.SchedulerSnapshotService
	if authorize {
		snapshot = service.NewSchedulerSnapshotService(nil, nil, accountRepo, groups, nil)
	}
	upstream, inner, cleanup := newCodexWireEntryWithSnapshot(t, accounts, usage, configure, schedulerSlots, snapshot, accountRepo)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		ctx := service.WithBPSRequestReauthorize(c.Request.Context(), func(check context.Context, requestedModel string) error {
			if err := check.Err(); err != nil {
				return err
			}
			if requestedModel != "gpt-5.4" {
				return errors.New("fixture model is not authorized")
			}
			return nil
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	router.Any("/*path", gin.WrapH(inner))
	return upstream, router, cleanup
}

func storedResponseDispatchAccount(id int64, local bool) service.Account {
	a := service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{9001},
		Credentials: map[string]any{"api_key": "sk-dispatch-test", "base_url": "https://gateway.example", "openai_capabilities": []string{"chat_completions", "responses"}},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)}}
	if local {
		a.Credentials["openai_capabilities"] = []string{"chat_completions", "responses", "bps_local_responses"}
		for k, v := range map[string]string{"bps_responses_contract": "local-responses/1", "bps_responses_control_url": "https://gateway.example", "bps_responses_service_key": "service-test", "bps_responses_backend_id": "backend-test", "bps_responses_subject_fingerprint": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "bps_responses_credential_generation": "generation-test"} {
			a.Credentials[k] = v
		}
	}
	return a
}

func TestStoredResponsesHTTPRejectsMixedPoolBeforeAnyExecution(t *testing.T) {
	for _, store := range []string{"", `,"store":true`, `,"store":false`} {
		t.Run(store, func(t *testing.T) {
			accounts := []service.Account{storedResponseDispatchAccount(14101, true), storedResponseDispatchAccount(14102, false)}
			repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
			upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
			defer cleanup()
			response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello"`+store+`}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			require.Equal(t, "response_storage_route_unavailable", gjson.GetBytes(response.Body.Bytes(), "error.code").String())
			require.Equal(t, 1, repo.listCalls)
			require.Empty(t, upstream.captures, "mixed storage domains must not preflight or generate")
		})
	}
}

func TestStoredResponsesHTTPOrdinaryPoolPreservesProviderStorageSemantics(t *testing.T) {
	for _, store := range []string{"", `,"store":true`, `,"store":false`} {
		t.Run(store, func(t *testing.T) {
			accounts := []service.Account{storedResponseDispatchAccount(14102, false)}
			repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
			upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
			defer cleanup()
			response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello"`+store+`}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, 1, repo.listCalls)
			require.Len(t, upstream.captures, 1)
			wire := upstream.captures[0]
			require.Equal(t, "/v1/responses", wire.path)
			actual := gjson.GetBytes(wire.body, "store")
			if store == "" {
				require.False(t, actual.Exists())
			} else {
				require.Equal(t, store == `,"store":true`, actual.Bool())
			}
			require.NotContains(t, response.Body.String(), "resp_local_")
		})
	}
}

func TestStoredResponsesHTTPExplicitFalsePreservesLocalToolsPath(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14101, true)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
	defer cleanup()
	response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello","store":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, upstream.captures, 1)
	require.False(t, gjson.GetBytes(upstream.captures[0].body, "store").Bool())
	require.Equal(t, "function", gjson.GetBytes(upstream.captures[0].body, "tools.0.type").String())
	require.NotContains(t, response.Body.String(), "resp_local_")
}

func TestStoredResponsesHTTPToolsDefaultStorageFailsBeforeExecution(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14101, true)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
	defer cleanup()
	response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Empty(t, upstream.captures)
}

func TestStoredResponsesHTTPFeatureDisabledDoesNotChangeOrdinaryDispatch(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14102, false), storedResponseDispatchAccount(14101, true)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, nil, false, repo)
	defer cleanup()
	response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Zero(t, repo.listCalls)
	require.Len(t, upstream.captures, 1)
}

func TestStoredResponsesHTTPDefaultStoreCreatesTrustedPlanAndFailsClosedWithoutStorage(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14101, true)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
	defer cleanup()
	var plan *service.StoredResponsePlan
	upstream.afterRequest = func(c *gin.Context) { plan = service.StoredResponsePlanFromContext(c.Request.Context()) }
	response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","instructions":"client original","input":"hello"}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.NotNil(t, plan)
	require.Equal(t, service.StoredResponseOwner{UserID: 9003, APIKeyID: 9002, GroupID: 9001}, plan.Owner)
	require.True(t, plan.Request.Store)
	require.Equal(t, "client original", gjson.GetBytes(plan.ClientRequest, "instructions").String())
	require.False(t, gjson.GetBytes(plan.ClientRequest, "store").Bool())
	require.Empty(t, upstream.captures, "unavailable storage must not execute inference")
}

func TestStoredResponsesHTTPStripsUntrustedPrivateProofHeaders(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14102, false)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, nil, false, repo)
	defer cleanup()
	private := []string{"X-Bps-Service-Key", "X-Bps-Owner-Ref", "X-Bps-Group-Ref", "X-Bps-Responses-Admission", "X-Bps-Responses-Wire-SHA256", "X-Bps-Responses-Binding-SHA256", "X-Bps-Responses-Admitted", "X-Bps-Responses-Unknown"}
	upstream.afterRequest = func(c *gin.Context) {
		for _, name := range private {
			require.Empty(t, c.Request.Header.Values(name), name)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	for _, name := range private {
		request.Header.Add(name, "untrusted")
		request.Header.Add(name, "duplicate")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, upstream.captures, 1)
	for _, name := range private {
		require.Empty(t, upstream.captures[0].header.Values(name), name)
	}
}

type storedDispatchFinalizeRepository struct {
	service.StoredResponseRepository
	reservations, finalizations int
}

func (r *storedDispatchFinalizeRepository) Reserve(_ context.Context, record *service.StoredResponse, _ service.StoredResponseLimits, _ time.Duration, _ time.Duration) error {
	r.reservations++
	record.Version = 1
	return nil
}
func (r *storedDispatchFinalizeRepository) Finalize(context.Context, string, string, int64, service.StoredResponseTerminal) (*service.StoredResponse, error) {
	r.finalizations++
	return nil, errors.New("fixture finalize failure")
}

func TestStoredResponsesHTTPFinalizeFailureWritesOneSafeTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			accounts := []service.Account{storedResponseDispatchAccount(14101, true)}
			repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
			upstream, router, cleanup := newStoredResponsesDispatchEntry(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, true, repo)
			defer cleanup()
			storeRepo := &storedDispatchFinalizeRepository{}
			storedConfig := service.DefaultStoredResponseConfig()
			storedConfig.Enabled = true
			stored, err := service.NewStoredResponseService(storeRepo, storedConfig)
			require.NoError(t, err)
			upstream.handler.gatewayService.SetStoredResponseService(stored)
			var preflight managedfiles.ResponsesPreflightResult
			preflightResponse := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}}
			upstream.afterPost = func() {
				preflightRequest, decodeErr := managedfiles.DecodeResponsesPreflight(upstream.captures[0].body)
				require.NoError(t, decodeErr)
				wireHash := managedfiles.ResponsesWireSHA256(preflightRequest.WireRequest)
				binding, bindingErr := managedfiles.ResponsesBindingSHA256(preflightRequest, "policy-1", wireHash)
				require.NoError(t, bindingErr)
				preflight = managedfiles.ResponsesPreflightResult{Version: managedfiles.ResponsesContract, Identity: preflightRequest.Expected, PolicyVersion: "policy-1", AdmissionID: "responses-" + strings.Repeat("a", 64), ExpiresAt: time.Now().Add(30 * time.Second).Unix(), WireSHA256: wireHash, BindingSHA256: binding}
				encoded, marshalErr := json.Marshal(preflight)
				require.NoError(t, marshalErr)
				// The current preflight queued response is returned by pointer.
				preflightResponse.Body = io.NopCloser(bytes.NewReader(encoded))
				header := http.Header{"Content-Type": {"application/json"}}
				header.Set(managedfiles.ResponsesAdmittedHeader, managedfiles.ResponsesAck("service-test", preflight))
				body := `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}`
				if stream {
					header.Set("Content-Type", "text/event-stream")
					body = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"gpt-5.4\",\"output\":[]}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
					body = strings.Replace(body, "event: response.completed", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_native\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"hello\"}\n\nevent: response.completed", 1)
				}
				upstream.postResponses = append(upstream.postResponses, &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))})
			}
			upstream.postResponses = []*http.Response{preflightResponse}
			body := fmt.Sprintf(`{"model":"gpt-5.4","input":"hello","stream":%t}`, stream)
			response := codexWireSend(t, router, "/v1/responses", body)
			require.Equal(t, 1, storeRepo.reservations)
			require.NotZero(t, storeRepo.finalizations)
			require.Len(t, upstream.captures, 2, "one preflight and one inference")
			require.NotContains(t, response.Body.String(), "resp_native")
			require.NotContains(t, response.Body.String(), "response.completed")
			if !stream {
				require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				require.True(t, json.Valid(response.Body.Bytes()), response.Body.String())
				require.Equal(t, "response_storage_error", gjson.GetBytes(response.Body.Bytes(), "error.code").String())
			} else {
				require.Equal(t, 1, strings.Count(response.Body.String(), "event: response.failed\n"), response.Body.String())
				require.Contains(t, response.Body.String(), "resp_local_")
			}
		})
	}
}

func TestStoredResponsesHTTPPreflightPolicyKeepsSafePublicError(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14101, true)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntry(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, true, repo)
	defer cleanup()
	storeRepo := &storedDispatchFinalizeRepository{}
	storedConfig := service.DefaultStoredResponseConfig()
	storedConfig.Enabled = true
	stored, err := service.NewStoredResponseService(storeRepo, storedConfig)
	require.NoError(t, err)
	upstream.handler.gatewayService.SetStoredResponseService(stored)
	upstream.postResponses = []*http.Response{{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}, "X-Bps-Request-Policy": {"rejected-v1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"unsupported_tool_choice","param":"tool_choice","message":"The requested capability is not supported by this gateway."}}`))}}
	response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello","tool_choice":"required"}`)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Equal(t, "unsupported_tool_choice", gjson.GetBytes(response.Body.Bytes(), "error.code").String())
	require.Equal(t, "tool_choice", gjson.GetBytes(response.Body.Bytes(), "error.param").String())
	require.Len(t, upstream.captures, 1, "one preflight, no inference or compatibility repair")
	require.Zero(t, storeRepo.reservations)
}

func TestStoredResponsesHTTPSlotAdmissionFencesCredentialAndDomainRotation(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			accounts := []service.Account{storedResponseDispatchAccount(14101, local)}
			repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
			upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, true, repo)
			defer cleanup()
			var acquisitions int
			upstream.cache.acquireAccountSlotFn = func(context.Context, int64, int, string) (bool, error) {
				acquisitions++
				changed := storedResponseDispatchAccount(14101, true)
				changed.Credentials["api_key"] = "sk-rotated-test"
				repo.accounts[0] = changed
				return true, nil
			}
			response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello","store":false}`)
			require.NotZero(t, acquisitions)
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			require.Equal(t, "response_storage_route_unavailable", gjson.GetBytes(response.Body.Bytes(), "error.code").String())
			require.Empty(t, upstream.captures, "rotation after slot must fail before preflight or execution")
		})
	}
}

func TestStoredResponsesHTTPRouteFailureAfterUserWaitIsSSETerminal(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14101, true), storedResponseDispatchAccount(14102, false)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
	defer cleanup()
	upstream.handler.concurrencyHelper.pingInterval = time.Millisecond
	var userAcquisitions int
	upstream.cache.acquireUserSlotFn = func(context.Context, int64, int, string) (bool, error) {
		userAcquisitions++
		return userAcquisitions > 1, nil
	}
	response := codexWireSend(t, router, "/v1/responses", `{"model":"gpt-5.4","input":"hello","stream":true}`)
	require.Greater(t, userAcquisitions, 1)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Header().Get("Content-Type"), "text/event-stream")
	require.Equal(t, 1, strings.Count(response.Body.String(), "event: response.failed\n"), response.Body.String())
	require.Contains(t, response.Body.String(), "response_storage_route_unavailable")
	require.Empty(t, upstream.captures)
}

func TestStoredResponsesHTTPPlanCopiesRequireSharedMemoryBudget(t *testing.T) {
	accounts := []service.Account{storedResponseDispatchAccount(14101, true)}
	repo := &storedResponseDispatchRepo{grokCredentialHandlerRepo: &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}}
	upstream, router, cleanup := newStoredResponsesDispatchEntryWithConfig(t, accounts, nil, func(c *config.Config) { c.Gateway.StoredResponses.Enabled = true }, false, repo)
	defer cleanup()
	body := `{"model":"gpt-5.4","input":"` + strings.Repeat("x", 4096) + `"}`
	limit := int64(len(body) + 1024)
	budget := pkghttputil.NewBodyMemoryBudget(limit)
	ctx, release := pkghttputil.WithRequestBodyMemoryBudget(context.Background(), budget)
	defer release()
	var plan *service.StoredResponsePlan
	upstream.afterRequest = func(c *gin.Context) { plan = service.StoredResponsePlanFromContext(c.Request.Context()) }
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Equal(t, "service_unavailable", gjson.GetBytes(response.Body.Bytes(), "error.type").String())
	require.Nil(t, plan, "copy budget must be reserved before normalization or retention")
	require.Empty(t, upstream.captures)
	release()
	probe, releaseProbe := pkghttputil.WithRequestBodyMemoryBudget(context.Background(), budget)
	defer releaseProbe()
	require.NoError(t, pkghttputil.ReserveRequestBodyMemory(probe, limit), "all handler reservations must be reusable")
}

func TestStoredResponsesHTTPAuthorizationErrorsRemainSafeAfterStreamStart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"revoked", service.ErrBPSRequestAuthorizationRevoked, http.StatusForbidden, "permission_denied"},
		{"unavailable", service.ErrBPSRequestAuthorizationUnavailable, http.StatusServiceUnavailable, "response_storage_unavailable"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", tc.name, stream), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				if stream {
					c.Header("Content-Type", "text/event-stream")
					_, _ = c.Writer.WriteString(": ping\n\n")
					c.Writer.Flush()
				}
				h := &OpenAIGatewayHandler{}
				h.handleStoredResponseDispatchError(c, fmt.Errorf("private authorization detail: %w", tc.err), stream)
				require.NotContains(t, recorder.Body.String(), "private authorization detail")
				if stream {
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed\n"), recorder.Body.String())
					require.Contains(t, recorder.Body.String(), tc.code)
				} else {
					require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
					require.Equal(t, tc.code, gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
				}
			})
		}
	}
}
