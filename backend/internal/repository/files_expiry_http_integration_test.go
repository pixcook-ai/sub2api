//go:build integration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFileLifecycleHTTPUploadRequiresOpenAIGroupWithoutLockingExistingFiles(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	id := f.upload(t, f.Owner, "before-platform-change.txt", "user_data", nil)
	_, err := integrationDB.ExecContext(context.Background(), `UPDATE groups SET platform=$2 WHERE id=$1`, f.DBGroupID, service.PlatformAnthropic)
	require.NoError(t, err)
	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	require.NoError(t, mp.WriteField("purpose", "user_data"))
	part, err := mp.CreateFormFile("file", "blocked.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("blocked"))
	require.NoError(t, err)
	require.NoError(t, mp.Close())
	status, data := f.request(t, http.MethodPost, "/v1/files", f.Owner, &body, mp.FormDataContentType())
	require.Equal(t, http.StatusForbidden, status, string(data))
	for _, path := range []string{"/v1/files", "/v1/files/" + id, "/v1/files/" + id + "/content"} {
		status, data = f.request(t, http.MethodGet, path, f.Rotated, nil, "")
		require.Equal(t, http.StatusOK, status, string(data))
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM gateway_files WHERE user_id=$1`, f.Owner.UserID).Scan(&count))
	require.Equal(t, 1, count, "rejected POST must reserve neither metadata nor quota")
}

func TestFileLifecycleRepositoryReadRechecksExpiryAfterLockWait(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	id := f.upload(t, f.Owner, "locked.txt", "user_data", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var rowID int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT id FROM gateway_files WHERE public_id=$1 FOR UPDATE`, id).Scan(&rowID))
	done := make(chan error, 1)
	go func() {
		_, err := NewFileRepository(integrationDB).AcquireRead(ctx, f.Owner.UserID, f.DBGroupID, id, "blocked-expiry-reader", time.Minute)
		done <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%public_id=$1%FOR UPDATE%')`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond)
	_, err = tx.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, rowID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-done, service.ErrFileNotFound)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_file_read_leases WHERE file_id=$1`, rowID).Scan(&count))
	require.Zero(t, count)
}

func TestFileLifecycleHTTPExpiryWithoutWorkerAndMultipartRetention(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	id := f.upload(t, f.Owner, "expiring.txt", "user_data", map[string]string{
		"expires_after[anchor]": "created_at", "expires_after[seconds]": "3600",
	})
	status, data := f.request(t, http.MethodGet, "/v1/files/"+id, f.Owner, nil, "")
	require.Equal(t, http.StatusOK, status, string(data))
	var obj struct {
		CreatedAt int64 `json:"created_at"`
		ExpiresAt int64 `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(data, &obj))
	require.Equal(t, int64(3600), obj.ExpiresAt-obj.CreatedAt)
	_, err := integrationDB.ExecContext(context.Background(), `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE public_id=$1`, id)
	require.NoError(t, err)
	for _, path := range []string{"/v1/files/" + id, "/v1/files/" + id + "/content"} {
		status, data = f.request(t, http.MethodGet, path, f.Rotated, nil, "")
		require.Equal(t, http.StatusNotFound, status, string(data))
	}
	status, data = f.request(t, http.MethodGet, "/v1/files", f.Owner, nil, "")
	require.Equal(t, http.StatusOK, status, string(data))
	var page struct{ Data []json.RawMessage }
	require.NoError(t, json.Unmarshal(data, &page))
	require.Empty(t, page.Data)
	var state string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT state FROM gateway_files WHERE public_id=$1`, id).Scan(&state))
	require.Equal(t, "ready", state, "visibility must not depend on the cleanup worker")
}
