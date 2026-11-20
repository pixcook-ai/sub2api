//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBPSRequestAuthorizationBindsLazyFreshCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := storedResponseAuthKeyForTest()
	calls := 0
	repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { calls++; clone := *key; return &clone, nil }}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
	var callback service.BPSRequestReauthorize
	router.POST("/v1/responses", func(c *gin.Context) {
		callback = service.BPSRequestReauthorizeFromContext(c.Request.Context())
		require.Equal(t, 1, calls, "binding must not eagerly add a database read")
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, callback, "a validated Responses request needs a server-owned fresh authorization port")
	key.Status = service.StatusDisabled
	require.ErrorIs(t, callback(context.Background(), "gpt-6.1-sol"), service.ErrBPSRequestAuthorizationRevoked)
	require.Equal(t, 2, calls, "use must bypass the initial authentication result")
}

func bpsRequestAuthorizationForTest(t *testing.T, key *service.APIKey, subscriptions *service.SubscriptionService, method, path string) (service.BPSRequestReauthorize, *stubApiKeyRepo, *http.Request, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	calls := 0
	repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { calls++; clone := *key; return &clone, nil }}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, subscriptions, cfg)))
	var callback service.BPSRequestReauthorize
	router.Handle(method, path, func(c *gin.Context) {
		callback = service.BPSRequestReauthorizeFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	return callback, repo, req, calls
}

func TestBPSRequestAuthorizationBindsOnlyResponsesPOST(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/responses", true}, {http.MethodPost, "/responses", true}, {http.MethodPost, "/backend-api/codex/responses", true}, {http.MethodPost, "/openai/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", true}, {http.MethodPost, "/responses/compact", true}, {http.MethodPost, "/backend-api/codex/responses/compact", true},
		{http.MethodGet, "/v1/responses", false}, {http.MethodGet, "/v1/responses/owned", false}, {http.MethodDelete, "/v1/responses/owned", false},
		{http.MethodPost, "/v1/files", false}, {http.MethodPost, "/v1/chat/completions", false}, {http.MethodPost, "/v1/responses/owned", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			callback, _, _, calls := bpsRequestAuthorizationForTest(t, storedResponseAuthKeyForTest(), nil, tc.method, tc.path)
			require.Equal(t, tc.want, callback != nil)
			require.Equal(t, 1, calls)
		})
	}
}

func TestBPSRequestAuthorizationRechecksCurrentKeyUserGroupModelIPAndBudgets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*service.APIKey)
		lookupErr error
		want      error
	}{
		{"valid", func(*service.APIKey) {}, nil, nil},
		{"deleted key", nil, service.ErrAPIKeyNotFound, service.ErrBPSRequestAuthorizationRevoked},
		{"database unavailable", nil, errors.New("private sk-secret https://internal.invalid/"), service.ErrBPSRequestAuthorizationUnavailable},
		{"changed key", func(k *service.APIKey) { k.ID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"changed user", func(k *service.APIKey) { k.UserID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"disabled key", func(k *service.APIKey) { k.Status = service.StatusDisabled }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"expired status", func(k *service.APIKey) { k.Status = service.StatusAPIKeyExpired }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"expired time", func(k *service.APIKey) { past := time.Now().Add(-time.Second); k.ExpiresAt = &past }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"quota exhausted status", func(k *service.APIKey) { k.Status = service.StatusAPIKeyQuotaExhausted }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"quota exhausted amount", func(k *service.APIKey) { k.Quota = 1; k.QuotaUsed = 1 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"disabled user", func(k *service.APIKey) { k.User.Status = service.StatusDisabled }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"missing user", func(k *service.APIKey) { k.User = nil }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"changed group", func(k *service.APIKey) { id := int64(999); k.GroupID = &id }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"missing group", func(k *service.APIKey) { k.Group = nil }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"disabled group", func(k *service.APIKey) { k.Group.Status = service.StatusDisabled }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"group permission revoked", func(k *service.APIKey) { k.User.RestrictPublicGroups = true }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"model removed", func(k *service.APIKey) {
			k.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"different-model"}}
		}, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"IP whitelist changed", func(k *service.APIKey) { k.IPWhitelist = []string{"198.51.100.1"} }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"IP blacklist changed", func(k *service.APIKey) { k.IPBlacklist = []string{"192.0.2.1"} }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"balance exhausted", func(k *service.APIKey) { k.User.Balance = 0 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"window budget exhausted", func(k *service.APIKey) { now := time.Now(); k.RateLimit5h = 1; k.Usage5h = 1; k.Window5hStart = &now }, nil, service.ErrBPSRequestAuthorizationRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := storedResponseAuthKeyForTest()
			callback, repo, _, _ := bpsRequestAuthorizationForTest(t, initial, nil, http.MethodPost, "/v1/responses")
			require.NotNil(t, callback)
			fresh := storedResponseAuthKeyForTest()
			if tc.change != nil {
				tc.change(fresh)
			}
			reads := 0
			repo.getByKey = func(context.Context, string) (*service.APIKey, error) { reads++; return fresh, tc.lookupErr }
			err := service.CheckBPSRequestAuthorization(service.WithBPSRequestReauthorize(context.Background(), callback), "gpt-6.1-sol")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
				require.NotContains(t, err.Error(), "sk-secret")
				require.NotContains(t, err.Error(), "internal.invalid")
			}
			require.Equal(t, 1, reads)
		})
	}
}

func TestBPSRequestAuthorizationUsesFrozenIngressIdentityAndOriginalModel(t *testing.T) {
	key := storedResponseAuthKeyForTest()
	callback, repo, req, _ := bpsRequestAuthorizationForTest(t, key, nil, http.MethodPost, "/v1/responses")
	fresh := storedResponseAuthKeyForTest()
	fresh.IPWhitelist = []string{"192.0.2.1"}
	fresh.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6.1-sol"}}
	repo.getByKey = func(context.Context, string) (*service.APIKey, error) { return fresh, nil }
	// A pooled Gin request may now represent another caller; only the ingress
	// security IP and authenticated IDs copied at binding time are evidence.
	req.RemoteAddr = "198.51.100.1:9876"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	key.ID++
	key.UserID++
	key.Group.ID++
	checkCtx := service.WithBPSRequestReauthorize(context.Background(), callback)
	require.NoError(t, service.CheckBPSRequestAuthorization(checkCtx, "gpt-6.1-sol"))
	require.ErrorIs(t, service.CheckBPSRequestAuthorization(checkCtx, "upstream-mapped-model"), service.ErrBPSRequestAuthorizationRevoked)
}

func TestBPSRequestAuthorizationRechecksSubscriptionWithoutCachedMembership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*service.UserSubscription)
		lookupErr error
		want      error
	}{
		{"active", func(*service.UserSubscription) {}, nil, nil},
		{"not found", nil, service.ErrSubscriptionNotFound, service.ErrBPSRequestAuthorizationRevoked},
		{"database unavailable", nil, errors.New("private subscription database"), service.ErrBPSRequestAuthorizationUnavailable},
		{"expired", func(s *service.UserSubscription) { s.ExpiresAt = time.Now().Add(-time.Second) }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"revoked", func(s *service.UserSubscription) { s.Status = service.SubscriptionStatusSuspended }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"wrong owner", func(s *service.UserSubscription) { s.UserID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"wrong group", func(s *service.UserSubscription) { s.GroupID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"deleted", func(s *service.UserSubscription) { now := time.Now(); s.DeletedAt = &now }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"at spending limit follows existing validation", func(s *service.UserSubscription) { s.DailyUsageUSD = 1 }, nil, nil},
		{"spending exceeded", func(s *service.UserSubscription) { s.DailyUsageUSD = 2 }, nil, service.ErrBPSRequestAuthorizationRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := storedResponseAuthKeyForTest()
			key.Group.SubscriptionType = service.SubscriptionTypeSubscription
			limit := 1.0
			key.Group.DailyLimitUSD = &limit
			now := time.Now()
			sub := &service.UserSubscription{ID: 88, UserID: key.UserID, GroupID: key.Group.ID, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Status: service.SubscriptionStatusActive, DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
			reads := 0
			var lookupErr error
			subscriptions := service.NewSubscriptionService(nil, &stubUserSubscriptionRepo{getActive: func(context.Context, int64, int64) (*service.UserSubscription, error) {
				reads++
				copy := *sub
				return &copy, lookupErr
			}}, nil, nil, &config.Config{RunMode: config.RunModeSimple})
			t.Cleanup(subscriptions.Stop)
			callback, _, _, _ := bpsRequestAuthorizationForTest(t, key, subscriptions, http.MethodPost, "/v1/responses")
			require.NotNil(t, callback)
			require.Zero(t, reads, "binding and simple-mode ingress must not eagerly query subscription")
			if tc.change != nil {
				tc.change(sub)
			}
			lookupErr = tc.lookupErr
			err := service.CheckBPSRequestAuthorization(service.WithBPSRequestReauthorize(context.Background(), callback), "gpt-6.1-sol")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.Equal(t, 1, reads, "subscription membership must come from the publication repository port")
		})
	}
}

func TestBPSRequestAuthorizationCancellationPreventsFreshLookup(t *testing.T) {
	callback, repo, _, _ := bpsRequestAuthorizationForTest(t, storedResponseAuthKeyForTest(), nil, http.MethodPost, "/v1/responses")
	reads := 0
	repo.getByKey = func(context.Context, string) (*service.APIKey, error) {
		reads++
		return nil, errors.New("must not read")
	}
	ctx, cancel := context.WithCancel(service.WithBPSRequestReauthorize(context.Background(), callback))
	cancel()
	require.ErrorIs(t, service.CheckBPSRequestAuthorization(ctx, "gpt-6.1-sol"), context.Canceled)
	require.Zero(t, reads)
}

func TestBPSRequestAuthorizationPreservesUnbilledFilePublication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := storedResponseAuthKeyForTest()
	key.Status = service.StatusAPIKeyQuotaExhausted
	key.Quota = 1
	key.QuotaUsed = 1
	key.User.Balance = 0
	key.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"unrelated-model"}}
	repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
	var publication func(context.Context) error
	router.POST("/v1/files", func(c *gin.Context) {
		publication = FileUploadReauthorize(c)
		require.Nil(t, service.BPSRequestReauthorizeFromContext(c.Request.Context()))
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/files", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, publication)
	require.NoError(t, publication(context.Background()), "publication remains unbilled and does not acquire inference model policy")
	key.IPBlacklist = []string{"192.0.2.1"}
	require.ErrorIs(t, publication(context.Background()), service.ErrFileAuthorizationRevoked, "the shared fresh IP authorization still applies to uploads")
}

func TestBPSRequestAuthorizationRejectsCurrentGroupPlatformDrift(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		want           error
	}{
		{"OpenAI retained", service.PlatformOpenAI, nil},
		{"composite retained for account fence", service.PlatformComposite, nil},
		{"drifted to native Anthropic", service.PlatformAnthropic, service.ErrBPSRequestAuthorizationRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callback, repo, _, _ := bpsRequestAuthorizationForTest(t, storedResponseAuthKeyForTest(), nil, http.MethodPost, "/v1/responses")
			fresh := storedResponseAuthKeyForTest()
			fresh.Group.Platform = tc.platform
			repo.getByKey = func(context.Context, string) (*service.APIKey, error) { return fresh, nil }
			err := service.CheckBPSRequestAuthorization(service.WithBPSRequestReauthorize(context.Background(), callback), "gpt-6.1-sol")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}
