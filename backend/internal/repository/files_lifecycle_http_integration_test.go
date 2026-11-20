//go:build integration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	httpserver "github.com/Wei-Shaw/sub2api/internal/server"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type lifecycleHTTPFixture struct {
	Config                  *config.Config
	DBGroupID               int64
	Owner, Rotated, Foreign *service.APIKey
	Files                   *service.FileService
	Store                   *service.PrivateFileBlobStore
	Subscriptions           *service.SubscriptionService
	Server                  *httptest.Server
}

func newLifecycleHTTPFixture(t *testing.T, requestTimeout ...time.Duration) *lifecycleHTTPFixture {
	t.Helper()
	ctx := context.Background()
	client := testEntClient(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	group, err := client.Group.Create().SetName("lifecycle-" + suffix).SetPlatform(service.PlatformOpenAI).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("lifecycle-" + suffix + "@test.invalid").SetPasswordHash("synthetic").SetBalance(0).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	other, err := client.User.Create().SetEmail("foreign-" + suffix + "@test.invalid").SetPasswordHash("synthetic").SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	keyRepo := NewAPIKeyRepository(client, integrationDB)
	key := func(uid int64, label string) *service.APIKey {
		k := &service.APIKey{UserID: uid, Key: "sk-lifecycle-" + label + suffix, Name: label, GroupID: &group.ID, Status: service.StatusActive}
		require.NoError(t, keyRepo.Create(ctx, k))
		return k
	}
	fc := config.DefaultFilesConfig()
	fc.Enabled = true
	fc.Root = t.TempDir()
	fc.FreeSpaceReserveBytes = 1
	cfg := &config.Config{Gateway: config.GatewayConfig{Files: fc}, Server: config.ServerConfig{MaxRequestBodySize: 1024}, SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 100, L1TTLSeconds: 60}}
	store, err := service.NewPrivateFileBlobStore(fc.Root, 1)
	require.NoError(t, err)
	files, err := service.NewFileService(NewFileRepository(integrationDB), store, fc)
	require.NoError(t, err)
	f := &lifecycleHTTPFixture{Config: cfg, DBGroupID: group.ID, Owner: key(user.ID, "owner"), Rotated: key(user.ID, "rotated"), Foreign: key(other.ID, "foreign"), Files: files, Store: store}
	r := gin.New()
	if len(requestTimeout) > 0 {
		r.Use(func(c *gin.Context) {
			ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout[0])
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
	}
	subscriptions := service.NewSubscriptionService(nil, NewUserSubscriptionRepository(client), nil, client, cfg)
	f.Subscriptions = subscriptions
	auth := middleware.NewAPIKeyAuthMiddleware(service.NewAPIKeyService(keyRepo, nil, nil, nil, nil, nil, cfg), subscriptions, cfg)
	routes.RegisterFileRoutes(r, handler.NewFileHandler(files), auth)
	f.Server = httptest.NewServer(httpserver.ProvideHTTPServer(cfg, r).Handler)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *lifecycleHTTPFixture) request(t *testing.T, method, path string, key *service.APIKey, body io.Reader, kind string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.Server.URL+path, body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	resp, err := f.Server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, data
}

func (f *lifecycleHTTPFixture) upload(t *testing.T, key *service.APIKey, name, purpose string, fields map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	mp := multipart.NewWriter(&buf)
	require.NoError(t, mp.WriteField("purpose", purpose))
	part, err := mp.CreateFormFile("file", name)
	require.NoError(t, err)
	_, err = part.Write([]byte("original " + name))
	require.NoError(t, err)
	for name, value := range fields {
		require.NoError(t, mp.WriteField(name, value))
	}
	require.NoError(t, mp.Close())
	status, data := f.request(t, http.MethodPost, "/v1/files", key, &buf, mp.FormDataContentType())
	require.Equal(t, http.StatusOK, status, string(data))
	var obj struct{ ID string }
	require.NoError(t, json.Unmarshal(data, &obj))
	require.NotEmpty(t, obj.ID)
	return obj.ID
}

func TestFileLifecycleHTTPListUsesOwnerScopedSDKCursor(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	a := f.upload(t, f.Owner, "a.txt", "user_data", nil)
	b := f.upload(t, f.Owner, "b.txt", "user_data", nil)
	c := f.upload(t, f.Owner, "c.txt", "user_data", nil)
	_ = f.upload(t, f.Owner, "assistant.txt", "assistants", nil)
	foreign := f.upload(t, f.Foreign, "foreign.txt", "user_data", nil)
	var page struct {
		Object  string
		Data    []struct{ ID string }
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
		HasMore bool    `json:"has_more"`
	}
	status, data := f.request(t, http.MethodGet, "/v1/files?limit=1&order=asc&purpose=user_data", f.Owner, nil, "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &page))
	require.Equal(t, "list", page.Object)
	require.Len(t, page.Data, 1)
	require.Equal(t, a, page.Data[0].ID)
	require.Equal(t, &a, page.FirstID)
	require.Equal(t, &a, page.LastID)
	require.True(t, page.HasMore)
	status, data = f.request(t, http.MethodGet, "/v1/files?limit=1&order=asc&purpose=user_data&after="+url.QueryEscape(a), f.Rotated, nil, "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &page))
	require.Equal(t, b, page.Data[0].ID)
	require.True(t, page.HasMore)
	status, data = f.request(t, http.MethodGet, "/v1/files?limit=1&order=asc&purpose=user_data&after="+url.QueryEscape(b), f.Owner, nil, "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &page))
	require.Equal(t, c, page.Data[0].ID)
	require.False(t, page.HasMore)
	status, data = f.request(t, http.MethodGet, "/v1/files?order=asc&purpose=user_data&after="+url.QueryEscape(c), f.Owner, nil, "")
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &page))
	require.Empty(t, page.Data)
	require.Nil(t, page.FirstID)
	require.Nil(t, page.LastID)
	require.False(t, page.HasMore)
	for _, query := range []string{"unknown=x", "limit=0", "limit=10001", "limit=1&limit=2", "order=", "order=sideways", "after="} {
		status, data = f.request(t, http.MethodGet, "/v1/files?"+query, f.Owner, nil, "")
		require.Equal(t, http.StatusBadRequest, status, string(data))
	}
	var invalid []byte
	for _, cursor := range []string{foreign, "file-does-not-exist"} {
		status, data = f.request(t, http.MethodGet, "/v1/files?after="+url.QueryEscape(cursor), f.Owner, nil, "")
		require.Equal(t, 400, status, string(data))
		if invalid == nil {
			invalid = data
		} else {
			require.JSONEq(t, string(invalid), string(data), "foreign/unknown cursor must be indistinguishable")
		}
	}
}
