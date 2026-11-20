package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type storedAdmissionAccounts struct {
	AccountRepository
	account *Account
}

func (r *storedAdmissionAccounts) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

type storedAdmissionUpstream struct {
	HTTPUpstream
	t                    *testing.T
	account              *Account
	db                   *captureTestDB
	preflight            int
	inference            int
	result               managedfiles.ResponsesPreflightResult
	wire                 []byte
	reject               bool
	badAck               bool
	bpsFailure           bool
	preflightDelay       time.Duration
	waitInferenceContext bool
}

func (u *storedAdmissionUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.t.Helper()
	raw, err := io.ReadAll(req.Body)
	require.NoError(u.t, err)
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if strings.HasSuffix(req.URL.Path, managedfiles.ResponsesPrefix+"preflight") {
		u.preflight++
		if u.preflightDelay > 0 {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(u.preflightDelay):
			}
		}
		require.Equal(u.t, 0, u.db.reservations, "preflight must precede durable reservation")
		require.Nil(u.t, req.GetBody)
		request, e := managedfiles.DecodeResponsesPreflight(raw)
		require.NoError(u.t, e)
		require.Equal(u.t, "1", request.OwnerRef)
		require.Equal(u.t, "3", request.GroupRef)
		require.False(u.t, gjson.GetBytes(request.ClientRequest, "store").Bool())
		if u.reject {
			header.Set("X-Bps-Request-Policy", "rejected-v1")
			return &http.Response{StatusCode: 400, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"max_output_tokens","message":"The requested capability is not supported by this gateway."}}`))}, nil
		}
		u.wire = append([]byte(nil), request.WireRequest...)
		hash := managedfiles.ResponsesWireSHA256(u.wire)
		binding, e := managedfiles.ResponsesBindingSHA256(request, "policy-1", hash)
		require.NoError(u.t, e)
		u.result = managedfiles.ResponsesPreflightResult{Version: managedfiles.ResponsesContract, Identity: request.Expected, PolicyVersion: "policy-1", AdmissionID: "responses-" + strings.Repeat("a", 64), ExpiresAt: time.Now().Add(30 * time.Second).Unix(), WireSHA256: hash, BindingSHA256: binding}
		encoded, e := json.Marshal(u.result)
		require.NoError(u.t, e)
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(bytes.NewReader(encoded))}, nil
	}
	u.inference++
	if u.waitInferenceContext {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(2 * time.Second):
			return nil, errors.New("fixture transport ceiling")
		}
	}
	require.Equal(u.t, 1, u.preflight)
	require.Equal(u.t, 1, u.db.reservations, "reserve must succeed before inference")
	require.Equal(u.t, u.wire, raw, "preflight must bind the exact final wire")
	require.Nil(u.t, req.GetBody)
	require.Empty(u.t, req.Header.Get("Content-Encoding"))
	require.Equal(u.t, u.result.AdmissionID, req.Header.Get(managedfiles.ResponsesAdmissionHeader))
	header.Set(managedfiles.ResponsesAdmittedHeader, managedfiles.ResponsesAck(u.account.GetCredential("bps_responses_service_key"), u.result))
	if u.badAck {
		header.Set(managedfiles.ResponsesAdmittedHeader, strings.Repeat("0", 64))
	}
	if u.bpsFailure {
		header.Set("X-Bps-Response-Contract", "failure-v1")
		return &http.Response{StatusCode: 429, Header: header, Body: io.NopCloser(strings.NewReader(bpsTestFailure))}, nil
	}
	return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(`{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}`))}, nil
}

func TestStoredResponseForwardPreflightsFinalWireBeforeReservationAndSingleSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := storedResponsesTestAccount(14001, 3)
	db := &captureTestDB{}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	stored, err := NewStoredResponseService(db, cfg)
	require.NoError(t, err)
	upstream := &storedAdmissionUpstream{t: t, account: &account, db: db}
	svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
	svc.SetStoredResponseService(stored)
	original := []byte(`{"model":"gpt-5.5","input":"hello","instructions":"client instructions"}`)
	plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
	require.NoError(t, err)
	ctx := WithStoredResponsePlan(storedAdmissionAuthorizedContext(context.Background()), plan)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
	result, err := svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.preflight)
	require.Equal(t, 1, upstream.inference)
	require.NotNil(t, result)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.True(t, strings.HasPrefix(result.ResponseID, "resp_local_"))
	require.NotNil(t, db.terminal)
	require.Equal(t, "completed", db.terminal.State)
	require.JSONEq(t, string(db.terminal.PublicResponse), recorder.Body.String())
	require.Empty(t, recorder.Header().Get(managedfiles.ResponsesAdmittedHeader))
}

func TestStoredResponseForwardRejectsBadAckWithoutReplayOrLosingObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := storedResponsesTestAccount(14002, 3)
	db := &captureTestDB{}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	stored, err := NewStoredResponseService(db, cfg)
	require.NoError(t, err)
	upstream := &storedAdmissionUpstream{t: t, account: &account, db: db, badAck: true}
	svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
	svc.SetStoredResponseService(stored)
	original, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "input": "hello"})
	plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
	require.NoError(t, err)
	ctx := WithStoredResponsePlan(storedAdmissionAuthorizedContext(context.Background()), plan)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
	result, err := svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
	var local *StoredResponseLocalError
	require.ErrorAs(t, err, &local)
	var failover *UpstreamFailoverError
	require.NotErrorAs(t, err, &failover)
	require.Equal(t, 1, upstream.preflight)
	require.Equal(t, 1, upstream.inference)
	require.Equal(t, 1, db.reservations)
	require.Equal(t, 503, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "resp_native")
	require.NotContains(t, recorder.Body.String(), "completed")
	require.Empty(t, recorder.Header().Get(managedfiles.ResponsesAdmittedHeader))
	require.NotNil(t, result)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.True(t, strings.HasPrefix(result.ResponseID, "resp_local_"))
	require.NotNil(t, db.terminal)
	require.Equal(t, "failed", db.terminal.State)
}

func TestStoredResponseForwardPreflightPolicyPreservesPublicErrorWithoutReservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := storedResponsesTestAccount(14003, 3)
	db := &captureTestDB{}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	stored, err := NewStoredResponseService(db, cfg)
	require.NoError(t, err)
	upstream := &storedAdmissionUpstream{t: t, account: &account, db: db, reject: true}
	svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
	svc.SetStoredResponseService(stored)
	original, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "input": "hello"})
	plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
	require.NoError(t, err)
	ctx := WithStoredResponsePlan(storedAdmissionAuthorizedContext(context.Background()), plan)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
	result, err := svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
	require.Nil(t, result)
	var local *StoredResponseLocalError
	require.ErrorAs(t, err, &local)
	var rejected *managedfiles.ResponsesPreflightError
	require.True(t, errors.As(err, &rejected))
	require.Equal(t, 400, rejected.Status)
	require.Equal(t, "unsupported_parameter", rejected.Code)
	require.Equal(t, "max_output_tokens", rejected.Param)
	require.Equal(t, 1, upstream.preflight)
	require.Zero(t, upstream.inference)
	require.Zero(t, db.reservations)
	require.Nil(t, db.terminal)
	require.False(t, recorder.Flushed)
}

func TestStoredResponseForwardKeepsHeaderDeadlineDuringAdmissionAndSend(t *testing.T) {
	for _, preflightSlow := range []bool{true, false} {
		t.Run(map[bool]string{true: "preflight", false: "inference"}[preflightSlow], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			account := storedResponsesTestAccount(14004, 3)
			db := &captureTestDB{}
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			stored, err := NewStoredResponseService(db, cfg)
			require.NoError(t, err)
			upstream := &storedAdmissionUpstream{t: t, account: &account, db: db, waitInferenceContext: !preflightSlow}
			if preflightSlow {
				upstream.preflightDelay = 1200 * time.Millisecond
			}
			serviceCfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
			serviceCfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
			svc := &OpenAIGatewayService{cfg: serviceCfg, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
			svc.SetStoredResponseService(stored)
			original := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
			plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
			require.NoError(t, err)
			ctx := WithStoredResponsePlan(storedAdmissionAuthorizedContext(context.Background()), plan)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
			started := time.Now()
			_, err = svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
			require.Error(t, err)
			require.Less(t, time.Since(started), 1500*time.Millisecond, "admission must retain the configured first-output cancellation")
			var local *StoredResponseLocalError
			require.ErrorAs(t, err, &local)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Equal(t, 1, upstream.preflight)
			if preflightSlow {
				require.Zero(t, upstream.inference)
				require.Zero(t, db.reservations)
			} else {
				require.Equal(t, 1, upstream.inference)
				require.Equal(t, 1, db.reservations)
				require.NotNil(t, db.terminal)
				require.Equal(t, "failed", db.terminal.State)
			}
		})
	}
}

func TestStoredResponseAdmissionRejectsWireMemoryOverloadBeforePreflight(t *testing.T) {
	account := storedResponsesTestAccount(14005, 3)
	db := &captureTestDB{}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	stored, err := NewStoredResponseService(db, cfg)
	require.NoError(t, err)
	upstream := &storedAdmissionUpstream{t: t, account: &account, db: db}
	svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
	svc.SetStoredResponseService(stored)
	original := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
	require.NoError(t, err)
	ctx, release := pkghttputil.WithRequestBodyMemoryBudget(storedAdmissionAuthorizedContext(context.Background()), pkghttputil.NewBodyMemoryBudget(1))
	defer release()
	req := httptest.NewRequest(http.MethodPost, "https://gateway.example/v1/responses", bytes.NewReader(plan.Request.UpstreamBody)).WithContext(ctx)
	_, _, cleanup, err := svc.admitStoredResponse(ctx, &account, req, plan)
	if cleanup != nil {
		_ = cleanup()
	}
	require.ErrorIs(t, err, pkghttputil.ErrRequestBodyMemoryBudget)
	require.Zero(t, upstream.preflight)
	require.Zero(t, upstream.inference)
	require.Zero(t, db.reservations)
	require.NoError(t, pkghttputil.ReserveRequestBodyMemory(ctx, 1), "failed scoped allocation must refund its reservation")
}

func TestStoredResponseForwardHTTPFailureCapturesAfterHeaderGuardBodyClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := storedResponsesTestAccount(14006, 3)
	db := &captureTestDB{}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	stored, err := NewStoredResponseService(&storedAdmissionContextDB{db}, cfg)
	require.NoError(t, err)
	upstream := &storedAdmissionUpstream{t: t, account: &account, db: db, bpsFailure: true}
	serviceCfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	serviceCfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	svc := &OpenAIGatewayService{cfg: serviceCfg, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
	svc.SetStoredResponseService(stored)
	original := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
	plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
	require.NoError(t, err)
	ctx := WithStoredResponsePlan(storedAdmissionAuthorizedContext(context.Background()), plan)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
	result, err := svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, 429, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "response_storage_error")
	require.NotContains(t, recorder.Body.String(), "resp_failed")
	require.NotNil(t, db.terminal)
	require.Equal(t, "failed", db.terminal.State)
	require.Contains(t, recorder.Body.String(), result.ResponseID)
	require.Equal(t, 1, upstream.preflight)
	require.Equal(t, 1, upstream.inference)
	require.Equal(t, 1, db.reservations)
}

// The real SQL repository honors cancellation; keep that boundary in the
// Forward tracer instead of allowing an in-memory fake to hide a cancelled CAS.
type storedAdmissionContextDB struct{ *captureTestDB }

func (d *storedAdmissionContextDB) Finalize(ctx context.Context, id, lease string, version int64, terminal StoredResponseTerminal) (*StoredResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.captureTestDB.Finalize(ctx, id, lease, version, terminal)
}

func storedAdmissionAuthorizedContext(ctx context.Context) context.Context {
	return WithBPSRequestReauthorize(ctx, func(ctx context.Context, model string) error { return ctx.Err() })
}

func storedAdmissionSchedulerSnapshot(groupID int64) *SchedulerSnapshotService {
	return &SchedulerSnapshotService{groupRepo: &managedAccountAuthorizationGroups{group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}}}
}

func TestStoredResponseAdmissionFreezesOriginalPublicModel(t *testing.T) {
	for _, composite := range []bool{false, true} {
		t.Run(map[bool]string{true: "composite-public-precedes-context-model", false: "context-model-precedes-wire"}[composite], func(t *testing.T) {
			account := storedResponsesTestAccount(14008, 3)
			account.Credentials["model_mapping"] = map[string]any{"public-alias": "gpt-5.5", "route-alias": "gpt-5.5"}
			db := &captureTestDB{}
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			stored, err := NewStoredResponseService(db, cfg)
			require.NoError(t, err)
			upstream := &storedAdmissionUpstream{t: t, account: &account, db: db}
			snapshot := storedAdmissionSchedulerSnapshot(3)
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: snapshot, httpUpstream: upstream}
			svc.SetStoredResponseService(stored)
			plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, []byte(`{"model":"gpt-5.5","input":"hello"}`))
			require.NoError(t, err)
			ctx := context.WithValue(context.Background(), ctxkey.Model, "public-alias")
			if composite {
				ctx = context.WithValue(ctx, ctxkey.Model, "route-alias")
				ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceExplicit, GroupID: 3, PublicModel: "public-alias", TargetPlatform: PlatformOpenAI, UpstreamModel: "route-alias", Endpoint: CompositeRouteEndpointResponses})
				routes := &managedAccountAuthorizationRoutes{routes: []CompositeModelRoute{{ID: 21, GroupID: 3, PublicModel: "public-alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "route-alias", Endpoint: CompositeRouteEndpointResponses, Enabled: true}}}
				checkedFixtureValue[*managedAccountAuthorizationGroups](snapshot.groupRepo).group.Platform = PlatformComposite
				ctx = WithManagedFileRouteResolver(ctx, NewCompositeRouteResolver(routes))
			}
			calls := 0
			ctx = WithBPSRequestReauthorize(ctx, func(_ context.Context, stringModel string) error {
				require.Equal(t, "public-alias", stringModel, "live authorization must never use a rewritten model")
				calls++
				return nil
			})
			req := httptest.NewRequest(http.MethodPost, "https://gateway.example/v1/responses", bytes.NewReader(plan.Request.UpstreamBody)).WithContext(ctx)
			admitted, _, cleanup, err := svc.admitStoredResponse(ctx, &account, req, plan)
			if cleanup != nil {
				defer func() { _ = cleanup() }()
			}
			require.NoError(t, err, "pure StoredResponses accounts do not require Files capability")
			// The authorization name is immutable even if a caller later changes its plan.
			plan.Request.Model = "later-mutation"
			require.NoError(t, claimStoredResponseGeneration(admitted.Context(), account.ID))
			require.Equal(t, 3, calls)
			require.Equal(t, 1, upstream.preflight)
			require.Equal(t, 1, db.reservations)
		})
	}
}

func TestStoredResponseGenerationRejectsCancellationDuringAuthorization(t *testing.T) {
	for _, cancelCaller := range []bool{true, false} {
		t.Run(map[bool]string{true: "original-caller", false: "send-context"}[cancelCaller], func(t *testing.T) {
			caller, cancelCallerContext := context.WithCancel(context.Background())
			defer cancelCallerContext()
			send, cancelSend := context.WithCancel(context.Background())
			defer cancelSend()
			state := &storedResponseGeneration{caller: caller, accountID: 14007, authorize: func(context.Context) error {
				if cancelCaller {
					cancelCallerContext()
				} else {
					cancelSend()
				}
				return nil
			}}
			send = context.WithValue(send, storedResponseGenerationKey{}, state)
			require.ErrorIs(t, claimStoredResponseGeneration(send, state.accountID), context.Canceled, "authorization cannot revive a cancelled caller or send")
		})
	}
}

func TestStoredResponseForwardRechecksLivePermissionBeforeEverySideEffect(t *testing.T) {
	for _, denyAt := range []int{0, 1, 2, 3} {
		t.Run(map[int]string{0: "missing-port", 1: "before-preflight", 2: "before-begin", 3: "before-send"}[denyAt], func(t *testing.T) {
			account := storedResponsesTestAccount(14007, 3)
			db := &captureTestDB{}
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			stored, err := NewStoredResponseService(db, cfg)
			require.NoError(t, err)
			upstream := &storedAdmissionUpstream{t: t, account: &account, db: db}
			svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}, accountRepo: &storedAdmissionAccounts{account: &account}, schedulerSnapshot: storedAdmissionSchedulerSnapshot(3), httpUpstream: upstream}
			svc.SetStoredResponseService(stored)
			original := []byte(`{"model":"gpt-5.5","input":"hello"}`)
			plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
			require.NoError(t, err)
			calls := 0
			ctx := context.Background()
			if denyAt > 0 {
				ctx = WithBPSRequestReauthorize(ctx, func(ctx context.Context, model string) error {
					require.Equal(t, "gpt-5.5", model, "authorization must use the original client model")
					calls++
					if calls == denyAt {
						return ErrBPSRequestAuthorizationRevoked
					}
					return ctx.Err()
				})
			}
			ctx = WithStoredResponsePlan(ctx, plan)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
			result, err := svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
			require.Nil(t, result)
			if denyAt == 0 {
				require.ErrorIs(t, err, ErrBPSRequestAuthorizationUnavailable)
			} else {
				require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
				require.Equal(t, denyAt, calls)
			}
			require.Zero(t, upstream.inference, "revocation must never reach inference")
			if denyAt <= 1 {
				require.Zero(t, upstream.preflight)
			} else {
				require.Equal(t, 1, upstream.preflight)
			}
			if denyAt < 3 {
				require.Zero(t, db.reservations)
				require.Nil(t, db.terminal)
			} else {
				require.Equal(t, 1, db.reservations)
				require.NotNil(t, db.terminal)
				require.Equal(t, "failed", db.terminal.State)
			}
		})
	}
}

func TestStoredResponseForwardRechecksCurrentBindingBeforeEverySideEffect(t *testing.T) {
	for _, change := range []string{"group-revoked", "effective-mapping-changed", "composite-route-changed"} {
		for _, changeAt := range []int{1, 2, 3} {
			t.Run(change+"/"+map[int]string{1: "before-preflight", 2: "before-begin", 3: "before-send"}[changeAt], func(t *testing.T) {
				account := storedResponsesTestAccount(14009, 3)
				current := storedResponsesTestAccount(account.ID, 3)
				account.Credentials["model_mapping"] = map[string]any{"gpt-5.5": "gpt-native", "gpt-route": "gpt-native"}
				current.Credentials["model_mapping"] = map[string]any{"gpt-5.5": "gpt-native", "gpt-route": "gpt-native"}
				db := &captureTestDB{}
				cfg := DefaultStoredResponseConfig()
				cfg.Enabled = true
				stored, err := NewStoredResponseService(db, cfg)
				require.NoError(t, err)
				upstream := &storedAdmissionUpstream{t: t, account: &account, db: db}
				snapshot := storedAdmissionSchedulerSnapshot(3)
				group := checkedFixtureValue[*managedAccountAuthorizationGroups](snapshot.groupRepo).group
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &storedAdmissionAccounts{account: &current}, schedulerSnapshot: snapshot, httpUpstream: upstream}
				svc.SetStoredResponseService(stored)
				original := []byte(`{"model":"gpt-5.5","input":"hello"}`)
				plan, err := NewStoredResponsePlan(StoredResponseOwner{1, 2, 3}, original)
				require.NoError(t, err)
				ctx := context.Background()
				routes := &managedAccountAuthorizationRoutes{}
				if change == "composite-route-changed" {
					group.Platform = PlatformComposite
					routes.routes = []CompositeModelRoute{{ID: 21, GroupID: 3, PublicModel: "gpt-5.5", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-route", Endpoint: CompositeRouteEndpointResponses, Enabled: true}}
					ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceExplicit, GroupID: 3, PublicModel: "gpt-5.5", TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-route", Endpoint: CompositeRouteEndpointResponses})
					ctx = WithManagedFileRouteResolver(ctx, NewCompositeRouteResolver(routes))
				}
				calls := 0
				ctx = WithBPSRequestReauthorize(ctx, func(context.Context, string) error {
					calls++
					if calls == changeAt {
						switch change {
						case "group-revoked":
							group.Status = StatusDisabled
						case "effective-mapping-changed":
							current.Credentials["model_mapping"] = map[string]any{"gpt-5.5": "gpt-other", "gpt-route": "gpt-native"}
						case "composite-route-changed":
							routes.routes[0].UpstreamModel = "gpt-other"
						}
					}
					return nil
				})
				ctx = WithStoredResponsePlan(ctx, plan)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)).WithContext(ctx)
				result, err := svc.Forward(ctx, c, &account, plan.Request.UpstreamBody)
				require.Nil(t, result)
				require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
				require.Equal(t, changeAt, calls)
				require.Zero(t, upstream.inference)
				if changeAt == 1 {
					require.Zero(t, upstream.preflight)
				} else {
					require.Equal(t, 1, upstream.preflight)
				}
				if changeAt < 3 {
					require.Zero(t, db.reservations)
				} else {
					require.Equal(t, 1, db.reservations)
					require.NotNil(t, db.terminal)
					require.Equal(t, "failed", db.terminal.State)
				}
			})
		}
	}
}
