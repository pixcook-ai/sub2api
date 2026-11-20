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
)

func TestStoredResponseResourceAuthAllowsQuotaExhaustedIdentityWithoutInferenceBalance(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			key := storedResponseAuthKeyForTest()
			key.Status = service.StatusAPIKeyQuotaExhausted
			key.User.Balance = 0
			status, callback := storedResponseAuthRequestForTest(t, config.RunModeStandard, key, nil, method, "/v1/responses/response-id")
			if status != http.StatusOK || callback {
				t.Fatalf("resource management used inference billing or upload reauthorization: status=%d callback=%t", status, callback)
			}
		})
	}
}

func TestStoredResponseResourceAuthKeepsInferenceAndSubresourceRequestsBilled(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/responses"},
		{http.MethodGet, "/v1/responses"},
		{http.MethodPost, "/v1/responses/response-id"},
		{http.MethodGet, "/v1/responses/response-id/input_items"},
		{http.MethodDelete, "/v1/responses/response-id/extra"},
	} {
		t.Run(tc.method+"/"+tc.path, func(t *testing.T) {
			key := storedResponseAuthKeyForTest()
			key.Status = service.StatusAPIKeyQuotaExhausted
			status, callback := storedResponseAuthRequestForTest(t, config.RunModeStandard, key, nil, tc.method, tc.path)
			if status != http.StatusTooManyRequests || callback {
				t.Fatalf("inference billing boundary changed: status=%d callback=%t", status, callback)
			}
		})
	}
}

func TestStoredResponseResourceAuthRejectsExpiredKeysEvenInSimpleMode(t *testing.T) {
	for _, mode := range []string{config.RunModeSimple, config.RunModeStandard} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			for _, stateExpired := range []bool{false, true} {
				t.Run(mode+"/"+method+"/expired-status="+map[bool]string{false: "false", true: "true"}[stateExpired], func(t *testing.T) {
					key := storedResponseAuthKeyForTest()
					if stateExpired {
						key.Status = service.StatusAPIKeyExpired
					} else {
						past := time.Now().Add(-time.Hour)
						key.ExpiresAt = &past
					}
					status, _ := storedResponseAuthRequestForTest(t, mode, key, nil, method, "/v1/responses/response-id")
					if status != http.StatusForbidden {
						t.Fatalf("expired identity reached management handler: %d", status)
					}
				})
			}
		}
	}
}

func TestStoredResponseResourceAuthRequiresSubscriptionMembershipWithoutSpendingLimits(t *testing.T) {
	for _, mode := range []string{config.RunModeSimple, config.RunModeStandard} {
		for _, tc := range []struct {
			name        string
			sub         *service.UserSubscription
			err         error
			unavailable bool
			want        int
		}{
			{name: "active with exhausted spending", sub: &service.UserSubscription{ID: 55, UserID: 7, GroupID: 42, Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour), DailyUsageUSD: 1000}, want: 200},
			{name: "no membership", err: service.ErrSubscriptionNotFound, want: 403},
			{name: "expired membership", sub: &service.UserSubscription{ID: 55, UserID: 7, GroupID: 42, Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(-time.Hour)}, want: 403},
			{name: "authorization unavailable", err: errors.New("synthetic database unavailable"), want: 503},
			{name: "no subscription service", unavailable: true, want: 503},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				key := storedResponseAuthKeyForTest()
				key.Group.SubscriptionType = service.SubscriptionTypeSubscription
				limit := 1.0
				key.Group.DailyLimitUSD = &limit
				var subscriptions *service.SubscriptionService
				if !tc.unavailable {
					cfg := &config.Config{RunMode: mode}
					repo := &stubUserSubscriptionRepo{getActive: func(context.Context, int64, int64) (*service.UserSubscription, error) { return tc.sub, tc.err }}
					subscriptions = service.NewSubscriptionService(nil, repo, nil, nil, cfg)
					t.Cleanup(subscriptions.Stop)
				}
				status, _ := storedResponseAuthRequestForTest(t, mode, key, subscriptions, http.MethodGet, "/v1/responses/response-id")
				if status != tc.want {
					t.Fatalf("membership/spending boundary: status=%d; want %d", status, tc.want)
				}
			})
		}
	}
}

func TestStoredResponseResourceAuthKeepsFilePublicationReauthorizationFilesOnly(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/files", true},
		{http.MethodGet, "/v1/files", false},
		{http.MethodGet, "/v1/responses/response-id", false},
		{http.MethodDelete, "/v1/responses/response-id", false},
	} {
		t.Run(tc.method+"/"+tc.path, func(t *testing.T) {
			status, callback := storedResponseAuthRequestForTest(t, config.RunModeSimple, storedResponseAuthKeyForTest(), nil, tc.method, tc.path)
			if status != http.StatusOK || callback != tc.want {
				t.Fatalf("publication callback: status=%d callback=%t want=%t", status, callback, tc.want)
			}
		})
	}
}

func storedResponseAuthKeyForTest() *service.APIKey {
	group := &service.Group{ID: 42, Name: "resource-test", Status: service.StatusActive, Platform: service.PlatformOpenAI, Hydrated: true}
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10, Concurrency: 3}
	return &service.APIKey{ID: 100, UserID: user.ID, Key: "resource-test-key", Status: service.StatusActive, User: user, Group: group, GroupID: &group.ID}
}

// This exercises the actual shared authentication middleware using its existing
// repository adapters; it does not claim real credential-cache or PG acceptance.
func storedResponseAuthRequestForTest(t *testing.T, mode string, key *service.APIKey, subscriptions *service.SubscriptionService, method, path string) (int, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: mode}
	repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { clone := *key; return &clone, nil }}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	r := gin.New()
	r.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, subscriptions, cfg)))
	callback := false
	r.Handle(method, path, func(c *gin.Context) { callback = FileUploadReauthorize(c) != nil; c.Status(http.StatusOK) })
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, callback
}
