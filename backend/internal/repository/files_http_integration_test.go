//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// This is the authenticated main HTTP seam: real migrated PostgreSQL,
// production APIKeyService/repository/auth, and private on-disk bytes.
func TestFilesHTTPPersistsAcrossRestartAndKeyRotation(t *testing.T) {
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	client := testEntClient(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	group, err := client.Group.Create().SetName("files-" + suffix).SetPlatform(service.PlatformOpenAI).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("files-" + suffix + "@test.invalid").SetPasswordHash("test-only").SetStatus(service.StatusActive).SetBalance(0).Save(ctx)
	require.NoError(t, err)
	other, err := client.User.Create().SetEmail("files-other-" + suffix + "@test.invalid").SetPasswordHash("test-only").SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	keyRepo := NewAPIKeyRepository(client, integrationDB)
	makeKey := func(uid int64, label string) *service.APIKey {
		key := &service.APIKey{UserID: uid, Key: "sk-files-" + label + suffix, Name: label, GroupID: &group.ID, Status: service.StatusActive}
		require.NoError(t, keyRepo.Create(ctx, key))
		return key
	}
	key, rotated, foreign := makeKey(user.ID, "first"), makeKey(user.ID, "rotated"), makeKey(other.ID, "foreign")
	fc := config.DefaultFilesConfig()
	fc.Enabled, fc.Root, fc.FreeSpaceReserveBytes = true, t.TempDir(), 1
	cfg := &config.Config{Server: config.ServerConfig{MaxRequestBodySize: 1024}, Gateway: config.GatewayConfig{Files: fc}}
	newServer := func() *httptest.Server {
		cfg.Gateway.Files = fc
		store, err := service.NewPrivateFileBlobStore(fc.Root, fc.FreeSpaceReserveBytes)
		require.NoError(t, err)
		files, err := service.NewFileService(NewFileRepository(integrationDB), store, fc)
		require.NoError(t, err)
		subscriptions := service.NewSubscriptionService(nil, NewUserSubscriptionRepository(client), nil, client, cfg)
		auth := middleware.NewAPIKeyAuthMiddleware(service.NewAPIKeyService(keyRepo, nil, nil, nil, nil, nil, cfg), subscriptions, cfg)
		r := gin.New()
		routes.RegisterFileRoutes(r, handler.NewFileHandler(files), auth)
		r.POST("/json-limit", func(c *gin.Context) {
			_, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.Status(http.StatusRequestEntityTooLarge)
				return
			}
			c.Status(http.StatusOK)
		})
		return httptest.NewServer(httpserver.ProvideHTTPServer(cfg, r).Handler)
	}
	request := func(s *httptest.Server, method, path, credential, contentType string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, s.URL+path, body)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+credential)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := s.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	payload := []byte("original bytes\nwith binary: \x00\xff")
	var upload bytes.Buffer
	mp := multipart.NewWriter(&upload)
	require.NoError(t, mp.WriteField("purpose", "user_data"))
	part, err := mp.CreateFormFile("file", "evidence.txt")
	require.NoError(t, err)
	_, err = part.Write(payload)
	require.NoError(t, err)
	require.NoError(t, mp.Close())
	s := newServer()
	resp := request(s, http.MethodPost, "/v1/files", key.Key, mp.FormDataContentType(), &upload)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))
	var file struct {
		ID, Object, Filename, Purpose string
		Bytes                         int64
		CreatedAt                     int64 `json:"created_at"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	require.Equal(t, "file", file.Object)
	require.Equal(t, "user_data", file.Purpose)
	require.Equal(t, int64(len(payload)), file.Bytes)
	require.NotEmpty(t, file.ID)
	require.NotContains(t, string(data), "processed", "local upload cannot claim upstream processing")
	s.Close()
	s = newServer()
	t.Cleanup(s.Close)
	for _, credential := range []string{key.Key, rotated.Key} {
		resp = request(s, http.MethodGet, "/v1/files/"+file.ID, credential, "", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp = request(s, http.MethodGet, "/v1/files/"+file.ID+"/content", credential, "", nil)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, payload, got)
		hash := sha256.Sum256(got)
		require.Equal(t, hex.EncodeToString(hash[:]), resp.Header.Get("X-Content-SHA256"))
	}
	resp = request(s, http.MethodGet, "/v1/files/"+file.ID, foreign.Key, "", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	t.Run("feature off rollback preserves durable resources", func(t *testing.T) {
		fc.Enabled = false
		disabled := newServer()
		resp := request(disabled, http.MethodGet, "/v1/files/"+file.ID, key.Key, "", nil)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		disabled.Close()
		fc.Enabled = true
		reenabled := newServer()
		defer reenabled.Close()
		resp = request(reenabled, http.MethodGet, "/v1/files/"+file.ID+"/content", rotated.Key, "", nil)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, payload, got, "rollback must preserve ID, durable record and original bytes")
	})

	t.Run("current authentication policy", func(t *testing.T) {
		for _, mode := range []string{"", config.RunModeSimple} {
			cfg.RunMode = mode
			_, err = client.APIKey.UpdateOneID(rotated.ID).SetStatus(service.StatusActive).SetExpiresAt(time.Now().Add(-time.Hour)).Save(ctx)
			require.NoError(t, err)
			expiredServer := newServer()
			resp = request(expiredServer, http.MethodGet, "/v1/files/"+file.ID, rotated.Key, "", nil)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
			require.Contains(t, string(body), `"error"`)
			require.Contains(t, string(body), "API_KEY_EXPIRED", "actual ExpiresAt must be enforced even when status remains active")
			expiredServer.Close()
		}
		cfg.RunMode = ""
		_, err = client.APIKey.UpdateOneID(rotated.ID).ClearExpiresAt().SetStatus(service.StatusAPIKeyQuotaExhausted).Save(ctx)
		require.NoError(t, err)
		quotaServer := newServer()
		resp = request(quotaServer, http.MethodGet, "/v1/files/"+file.ID+"/content", rotated.Key, "", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "quota exhaustion must not trap already-owned data")
		quotaServer.Close()
		for _, tc := range []struct {
			name   string
			change func()
			reset  func()
			want   int
		}{
			{"disabled key", func() { _, err = client.APIKey.UpdateOneID(rotated.ID).SetStatus(service.StatusDisabled).Save(ctx) }, func() { _, err = client.APIKey.UpdateOneID(rotated.ID).SetStatus(service.StatusActive).Save(ctx) }, 401},
			{"disabled user", func() { _, err = client.User.UpdateOneID(user.ID).SetStatus(service.StatusDisabled).Save(ctx) }, func() { _, err = client.User.UpdateOneID(user.ID).SetStatus(service.StatusActive).Save(ctx) }, 401},
			{"IP ACL", func() { _, err = client.APIKey.UpdateOneID(rotated.ID).SetIPWhitelist([]string{"192.0.2.1"}).Save(ctx) }, func() { _, err = client.APIKey.UpdateOneID(rotated.ID).SetIPWhitelist([]string{}).Save(ctx) }, 403},
			{"disabled group", func() { _, err = client.Group.UpdateOneID(group.ID).SetStatus(service.StatusDisabled).Save(ctx) }, func() { _, err = client.Group.UpdateOneID(group.ID).SetStatus(service.StatusActive).Save(ctx) }, 403},
			{"group permission revoked", func() { _, err = client.User.UpdateOneID(user.ID).SetRestrictPublicGroups(true).Save(ctx) }, func() { _, err = client.User.UpdateOneID(user.ID).SetRestrictPublicGroups(false).Save(ctx) }, 403},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.change()
				require.NoError(t, err)
				defer func() { tc.reset(); require.NoError(t, err) }()
				blocked := newServer()
				defer blocked.Close()
				resp = request(blocked, http.MethodGet, "/v1/files/"+file.ID, rotated.Key, "", nil)
				require.Equal(t, tc.want, resp.StatusCode)
			})
		}
	})

	makeUpload := func(filename, purpose string, payload []byte, purposeLast bool) (*bytes.Buffer, string) {
		t.Helper()
		var b bytes.Buffer
		mp := multipart.NewWriter(&b)
		if !purposeLast {
			require.NoError(t, mp.WriteField("purpose", purpose))
		}
		part, err := mp.CreateFormFile("file", filename)
		require.NoError(t, err)
		_, err = part.Write(payload)
		require.NoError(t, err)
		if purposeLast {
			require.NoError(t, mp.WriteField("purpose", purpose))
		}
		require.NoError(t, mp.Close())
		return &b, mp.FormDataContentType()
	}
	t.Run("streaming input boundaries", func(t *testing.T) {
		for _, tc := range []struct {
			name, filename, purpose string
			last                    bool
			payload                 []byte
			limit                   int64
			want                    int
		}{
			{"purpose after file", "after.txt", "user_data", true, []byte("after"), 200 << 20, 200},
			{"file above global JSON limit", "independent.txt", "user_data", false, bytes.Repeat([]byte("x"), 4096), 200 << 20, 200},
			{"unsafe filename", "../escape.txt", "user_data", false, []byte("x"), 200 << 20, 400},
			{"unsupported purpose", "purpose.txt", "unknown-purpose", false, []byte("x"), 200 << 20, 400},
			{"empty file", "empty.txt", "user_data", false, nil, 200 << 20, 400},
			{"streaming file maximum", "large.txt", "user_data", false, bytes.Repeat([]byte("x"), 17), 16, 413},
		} {
			t.Run(tc.name, func(t *testing.T) {
				old := fc.MaxFileBytes
				fc.MaxFileBytes = tc.limit
				defer func() { fc.MaxFileBytes = old }()
				server := newServer()
				defer server.Close()
				body, kind := makeUpload(tc.filename, tc.purpose, tc.payload, tc.last)
				resp := request(server, http.MethodPost, "/v1/files", key.Key, kind, body)
				got, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, tc.want, resp.StatusCode, string(got))
				entries, err := os.ReadDir(filepath.Join(fc.Root, "staging"))
				require.NoError(t, err)
				require.Empty(t, entries, "rejected uploads must leave no staging bytes")
			})
		}
		resp := request(s, http.MethodPost, "/json-limit", key.Key, "application/json", bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, "Files must not increase the existing JSON body limit")
	})
	t.Run("actual database failure cleans private bytes", func(t *testing.T) {
		_, err := integrationDB.ExecContext(ctx, `CREATE FUNCTION files_http_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RETURN NEW; END $$;
 CREATE TRIGGER files_http_fault BEFORE INSERT OR UPDATE ON gateway_files FOR EACH ROW EXECUTE FUNCTION files_http_fault()`)
		require.NoError(t, err)
		defer func() {
			_, err := integrationDB.ExecContext(context.Background(), `DROP TRIGGER files_http_fault ON gateway_files; DROP FUNCTION files_http_fault()`)
			require.NoError(t, err)
		}()
		for _, filename := range []string{"reject-reserve.txt", "reject-commit.txt"} {
			condition := "NEW.state='ready'"
			if filename == "reject-reserve.txt" {
				condition = "TG_OP='INSERT'"
			}
			_, err = integrationDB.ExecContext(ctx, `CREATE OR REPLACE FUNCTION files_http_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF `+condition+` THEN RAISE EXCEPTION 'Files acceptance database write fault'; END IF; RETURN NEW; END $$`)
			require.NoError(t, err)
			server := newServer()
			body, kind := makeUpload(filename, "user_data", []byte("private failure evidence"), false)
			resp := request(server, http.MethodPost, "/v1/files", key.Key, kind, body)
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, 503, resp.StatusCode, string(got))
			server.Close()
			var live int
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_files WHERE user_id=$1 AND filename=$2 AND state<>'deleted'`, user.ID, filename).Scan(&live))
			require.Zero(t, live)
		}
		entries, err := os.ReadDir(filepath.Join(fc.Root, "staging"))
		require.NoError(t, err)
		require.Empty(t, entries)
	})
	t.Run("client cancellation cleans staging and budget", func(t *testing.T) {
		server := newServer()
		defer server.Close()
		rd, wr := io.Pipe()
		mp := multipart.NewWriter(wr)
		requestCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL+"/v1/files", rd)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Content-Type", mp.FormDataContentType())
		done := make(chan error, 1)
		go func() {
			resp, err := server.Client().Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		go func() {
			if mp.WriteField("purpose", "user_data") != nil {
				return
			}
			part, err := mp.CreateFormFile("file", "cancelled.txt")
			if err != nil {
				return
			}
			_, _ = part.Write(bytes.Repeat([]byte("x"), 64*1024))
		}()
		var storageKey string
		require.Eventually(t, func() bool {
			return integrationDB.QueryRowContext(ctx, `SELECT storage_key FROM gateway_files WHERE user_id=$1 AND state='staging' ORDER BY id DESC LIMIT 1`, user.ID).Scan(&storageKey) == nil
		}, 5*time.Second, 10*time.Millisecond)
		cancel()
		_ = wr.CloseWithError(context.Canceled)
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled HTTP upload did not stop")
		}
		require.Eventually(t, func() bool {
			var state string
			return integrationDB.QueryRowContext(ctx, `SELECT state FROM gateway_files WHERE storage_key=$1`, storageKey).Scan(&state) == nil && state == "deleted"
		}, 5*time.Second, 10*time.Millisecond)
		_, err = os.Stat(filepath.Join(fc.Root, "staging", storageKey))
		require.True(t, os.IsNotExist(err))
	})
	t.Run("startup recovers rename before database commit", func(t *testing.T) {
		repo := NewFileRepository(integrationDB)
		store, err := service.NewPrivateFileBlobStore(fc.Root, 1)
		require.NoError(t, err)
		storageKey := strings.Repeat("e", 32)
		record := &service.StoredFile{PublicID: "file-" + storageKey, StorageKey: storageKey, UserID: user.ID, APIKeyID: key.ID, GroupID: group.ID, Filename: "crash.txt", MIME: "text/plain", ReservedBytes: fc.MaxFileBytes, UploadToken: "old-process"}
		require.NoError(t, repo.Reserve(ctx, record, fc.UserQuotaBytes, fc.GlobalQuotaBytes, time.Minute))
		_, _, err = store.Stage(ctx, storageKey, strings.NewReader("uncommitted durable bytes"), fc.MaxFileBytes)
		require.NoError(t, err)
		require.NoError(t, store.Commit(ctx, storageKey))
		// A still-live upload lease must survive another process' startup.
		cfg.Gateway.Files = fc
		_, err = service.ProvideFileService(repo, cfg)
		require.NoError(t, err)
		reader, err := store.Open(ctx, storageKey)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET upload_lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, record.ID)
		require.NoError(t, err)
		_, err = service.ProvideFileService(repo, cfg)
		require.NoError(t, err)
		_, err = store.Open(ctx, storageKey)
		require.True(t, os.IsNotExist(err))
		var state string
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state FROM gateway_files WHERE id=$1`, record.ID).Scan(&state))
		require.Equal(t, "deleted", state)
		_, err = repo.GetForOwner(ctx, user.ID, group.ID, record.PublicID)
		require.ErrorIs(t, err, service.ErrFileNotFound)
	})
	t.Run("subscription entitlement remains current", func(t *testing.T) {
		_, err := client.Group.UpdateOneID(group.ID).SetSubscriptionType(service.SubscriptionTypeSubscription).SetDailyLimitUsd(1).Save(ctx)
		require.NoError(t, err)
		defer func() {
			_, err := client.Group.UpdateOneID(group.ID).SetSubscriptionType(service.SubscriptionTypeStandard).Save(ctx)
			require.NoError(t, err)
		}()
		check := func(want int) {
			t.Helper()
			for _, mode := range []string{"", config.RunModeSimple} {
				cfg.RunMode = mode
				server := newServer()
				resp := request(server, http.MethodGet, "/v1/files/"+file.ID, key.Key, "", nil)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, want, resp.StatusCode, string(body))
				upload, kind := makeUpload("subscription.txt", "user_data", []byte("member data"), false)
				resp = request(server, http.MethodPost, "/v1/files", key.Key, kind, upload)
				require.Equal(t, want, resp.StatusCode)
				server.Close()
			}
			cfg.RunMode = ""
		}
		check(http.StatusForbidden)
		sub, err := client.UserSubscription.Create().SetUserID(user.ID).SetGroupID(group.ID).SetStartsAt(time.Now().Add(-time.Hour)).SetExpiresAt(time.Now().Add(time.Hour)).SetStatus(service.SubscriptionStatusActive).SetDailyUsageUsd(10).Save(ctx)
		require.NoError(t, err)
		check(http.StatusOK) // Exhausted inference budget must not deny storage management.
		_, err = client.UserSubscription.UpdateOneID(sub.ID).SetStatus(service.SubscriptionStatusExpired).Save(ctx)
		require.NoError(t, err)
		check(http.StatusForbidden)
		_, err = client.UserSubscription.UpdateOneID(sub.ID).SetStatus(service.SubscriptionStatusActive).SetExpiresAt(time.Now().Add(-time.Hour)).Save(ctx)
		require.NoError(t, err)
		check(http.StatusForbidden)
	})
}
