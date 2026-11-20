//go:build integration

package middleware

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// BPS_AUTH_TEST_POSTGRES_DSN must point to an isolated disposable database.
// Without it, use the repository suite's PostgreSQL image and full migrations.
func openBPSAuthorizationPostgres(t *testing.T) (*sql.DB, *dbent.Client) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("BPS_AUTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		container, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23", tcpostgres.WithDatabase("bps_auth_test"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
		require.NoError(t, err, "real PostgreSQL is required; this test must not silently skip")
		t.Cleanup(func() { require.NoError(t, container.Terminate(ctx)) })
		dsn, err = container.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
		require.NoError(t, err)
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(ctx))
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	return db, client
}

type bpsPostgresAuthorizationFixture struct {
	db            *sql.DB
	key           *service.APIKey
	keys          *service.APIKeyService
	subscriptions *service.SubscriptionService
	subscription  *service.UserSubscription
	reauthorize   service.BPSRequestReauthorize
}

func newBPSPostgresAuthorizationFixture(t *testing.T, db *sql.DB, client *dbent.Client, subscription bool, exclusive bool) *bpsPostgresAuthorizationFixture {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	groupBuilder := client.Group.Create().SetName("bps-fresh-" + suffix).SetPlatform(service.PlatformOpenAI).SetStatus(service.StatusActive).SetIsExclusive(exclusive).SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6.1-sol"}})
	if subscription {
		groupBuilder.SetSubscriptionType(service.SubscriptionTypeSubscription).SetDailyLimitUsd(1)
	}
	group, err := groupBuilder.Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("bps-fresh-" + suffix + "@test.invalid").SetPasswordHash("test-only").SetStatus(service.StatusActive).SetBalance(10).Save(ctx)
	require.NoError(t, err)
	if exclusive {
		_, err = client.UserAllowedGroup.Create().SetUserID(user.ID).SetGroupID(group.ID).Save(ctx)
		require.NoError(t, err)
	}
	repo := repository.NewAPIKeyRepository(client, db)
	key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-bps-fresh-" + suffix, Name: "fresh authorization", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))
	cfg := &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{L1Size: 64, L1TTLSeconds: 300}, SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 64, L1TTLSeconds: 300}}
	f := &bpsPostgresAuthorizationFixture{db: db, key: key, keys: service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)}
	t.Cleanup(f.keys.StopAuthCacheInvalidationSubscriber)
	if subscription {
		now := time.Now()
		subRepo := repository.NewUserSubscriptionRepository(client)
		f.subscription = &service.UserSubscription{UserID: user.ID, GroupID: group.ID, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Status: service.SubscriptionStatusActive, DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
		require.NoError(t, subRepo.Create(ctx, f.subscription))
		f.subscriptions = service.NewSubscriptionService(nil, subRepo, nil, client, cfg)
		t.Cleanup(f.subscriptions.Stop)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(f.keys, f.subscriptions, cfg)))
	router.POST("/v1/responses", func(c *gin.Context) {
		f.reauthorize = service.BPSRequestReauthorizeFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, f.reauthorize)
	// A canceled context cannot query PostgreSQL. Success proves the actual
	// asynchronous Ristretto cache is primed, without test-only production ports.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.Eventually(t, func() bool {
		_, err := f.keys.GetByKey(canceled, key.Key)
		return err == nil
	}, 5*time.Second, time.Millisecond)
	if subscription {
		require.Eventually(t, func() bool {
			_, err := f.subscriptions.GetActiveSubscription(canceled, user.ID, group.ID)
			return err == nil
		}, 5*time.Second, time.Millisecond)
	}
	require.NoError(t, f.reauthorize(ctx, "gpt-6.1-sol"), "unchanged owning rows authorize the requested client model")
	return f
}

func (f *bpsPostgresAuthorizationFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(), query, args...)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows, "fixture mutation must affect its owning row")
}

func TestBPSRequestAuthorizationPostgresBypassesPrimedCaches(t *testing.T) {
	db, client := openBPSAuthorizationPostgres(t)
	for _, tc := range []struct {
		name, query string
		owner       string
		exclusive   bool
	}{
		{"key disabled", "UPDATE api_keys SET status='disabled' WHERE id=$1", "key", false},
		{"key deleted", "UPDATE api_keys SET deleted_at=NOW() WHERE id=$1", "key", false},
		{"key expired", "UPDATE api_keys SET expires_at=NOW()-INTERVAL '1 hour' WHERE id=$1", "key", false},
		{"key quota exhausted", "UPDATE api_keys SET quota=1, quota_used=1 WHERE id=$1", "key", false},
		{"key quota exhausted status", "UPDATE api_keys SET status='quota_exhausted' WHERE id=$1", "key", false},
		{"key owner changed", "WITH replacement AS (INSERT INTO users (email,password_hash,status,balance) VALUES ('bps-replacement-' || $1::bigint::text || '@test.invalid','test-only','active',10) RETURNING id) UPDATE api_keys SET user_id=(SELECT id FROM replacement) WHERE id=$1", "key", false},
		{"key group removed", "UPDATE api_keys SET group_id=NULL WHERE id=$1", "key", false},
		{"IP whitelist changed", `UPDATE api_keys SET ip_whitelist='["198.51.100.1"]'::jsonb WHERE id=$1`, "key", false},
		{"IP blacklist changed", `UPDATE api_keys SET ip_blacklist='["192.0.2.1"]'::jsonb WHERE id=$1`, "key", false},
		{"five hour budget exhausted", "UPDATE api_keys SET rate_limit_5h=1, usage_5h=1, window_5h_start=NOW() WHERE id=$1", "key", false},
		{"daily budget exhausted", "UPDATE api_keys SET rate_limit_1d=1, usage_1d=1, window_1d_start=NOW() WHERE id=$1", "key", false},
		{"weekly budget exhausted", "UPDATE api_keys SET rate_limit_7d=1, usage_7d=1, window_7d_start=NOW() WHERE id=$1", "key", false},
		{"user disabled", "UPDATE users SET status='disabled' WHERE id=$1", "user", false},
		{"user deleted", "UPDATE users SET deleted_at=NOW() WHERE id=$1", "user", false},
		{"balance exhausted", "UPDATE users SET balance=0 WHERE id=$1", "user", false},
		{"public group permission revoked", "UPDATE users SET restrict_public_groups=true WHERE id=$1", "user", false},
		{"exclusive group permission revoked", "DELETE FROM user_allowed_groups WHERE user_id=$1", "user", true},
		{"group disabled", "UPDATE groups SET status='disabled' WHERE id=$1", "group", false},
		{"group deleted", "UPDATE groups SET deleted_at=NOW() WHERE id=$1", "group", false},
		{"group platform changed", "UPDATE groups SET platform='anthropic' WHERE id=$1", "group", false},
		{"requested model removed", `UPDATE groups SET model_allowlist='{"enabled":true,"models":["different-model"]}'::jsonb WHERE id=$1`, "group", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBPSPostgresAuthorizationFixture(t, db, client, false, tc.exclusive)
			before, err := f.keys.GetByKey(context.Background(), f.key.Key)
			require.NoError(t, err)
			lookups := f.keys.AuthLookupMetrics().Total
			id := f.key.ID
			if tc.owner == "user" {
				id = f.key.UserID
			} else if tc.owner == "group" {
				id = *f.key.GroupID
			}
			f.exec(t, tc.query, id)
			cached, err := f.keys.GetByKey(context.Background(), f.key.Key)
			require.NoError(t, err)
			require.Equal(t, before, cached, "the ingress auth snapshot must remain stale after direct SQL")
			require.Equal(t, lookups, f.keys.AuthLookupMetrics().Total)
			require.ErrorIs(t, f.reauthorize(context.Background(), "gpt-6.1-sol"), service.ErrBPSRequestAuthorizationRevoked)
		})
	}
	for _, tc := range []struct{ name, query string }{
		{"suspended", "UPDATE user_subscriptions SET status='suspended' WHERE id=$1"},
		{"expired", "UPDATE user_subscriptions SET expires_at=NOW()-INTERVAL '1 hour' WHERE id=$1"},
		{"deleted", "UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=$1"},
		{"spending exceeded", "UPDATE user_subscriptions SET daily_usage_usd=2 WHERE id=$1"},
	} {
		t.Run("subscription "+tc.name, func(t *testing.T) {
			f := newBPSPostgresAuthorizationFixture(t, db, client, true, false)
			f.exec(t, tc.query, f.subscription.ID)
			cached, err := f.subscriptions.GetActiveSubscription(context.Background(), f.key.UserID, *f.key.GroupID)
			require.NoError(t, err)
			require.True(t, cached.IsActive(), "actual subscription L1 still grants stale membership")
			require.Zero(t, cached.DailyUsageUSD)
			require.ErrorIs(t, f.reauthorize(context.Background(), "gpt-6.1-sol"), service.ErrBPSRequestAuthorizationRevoked)
		})
	}
	t.Run("fresh database unavailable despite cached ingress", func(t *testing.T) {
		f := newBPSPostgresAuthorizationFixture(t, db, client, false, false)
		_, err := db.Exec("ALTER TABLE api_keys RENAME TO bps_authorization_unavailable_keys")
		require.NoError(t, err)
		defer func() {
			_, err := db.Exec("ALTER TABLE bps_authorization_unavailable_keys RENAME TO api_keys")
			require.NoError(t, err)
		}()
		_, err = f.keys.GetByKey(context.Background(), f.key.Key)
		require.NoError(t, err, "cached ingress still succeeds with unavailable key relation")
		require.ErrorIs(t, f.reauthorize(context.Background(), "gpt-6.1-sol"), service.ErrBPSRequestAuthorizationUnavailable)
	})
}
