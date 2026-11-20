//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFileLifecycleHTTPFirstBoundaryReservesSlotAndTimeoutReleasesIt(t *testing.T) {
	f := newLifecycleHTTPFixture(t, 300*time.Millisecond)
	reader, writer := io.Pipe()
	defer writer.Close()
	req, err := http.NewRequest(http.MethodPost, f.Server.URL+"/v1/files", reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.Owner.Key)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=slow-boundary")
	response := make(chan *http.Response, 1)
	failure := make(chan error, 1)
	go func() {
		resp, err := f.Server.Client().Do(req)
		if err != nil {
			failure <- err
		} else {
			response <- resp
		}
	}()
	require.Eventually(t, func() bool {
		var n int
		err := integrationDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM gateway_files WHERE user_id=$1 AND state='staging'`, f.Owner.UserID).Scan(&n)
		return err == nil && n == 1
	}, 200*time.Millisecond, 5*time.Millisecond, "reservation must precede first multipart boundary")
	select {
	case err := <-failure:
		require.NoError(t, err)
	case resp := <-response:
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 408, resp.StatusCode, string(body))
		var result struct{ Error struct{ Code string } }
		require.NoError(t, json.Unmarshal(body, &result))
		require.Equal(t, "file_upload_timeout", result.Error.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("first-boundary timeout did not interrupt body read")
	}
	var state string
	var charge int64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT state,reserved_bytes FROM gateway_files WHERE user_id=$1`, f.Owner.UserID).Scan(&state, &charge))
	require.Equal(t, "deleted", state)
	require.Zero(t, charge)
	_ = f.upload(t, f.Rotated, "after-timeout.txt", "user_data", nil)
}

func TestFileLifecycleHTTPRevokedKeyCannotPublishAfterStreaming(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	reader, writer := io.Pipe()
	mp := multipart.NewWriter(writer)
	req, err := http.NewRequest(http.MethodPost, f.Server.URL+"/v1/files", reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.Owner.Key)
	req.Header.Set("Content-Type", mp.FormDataContentType())
	done := make(chan *http.Response, 1)
	fail := make(chan error, 1)
	go func() {
		resp, err := f.Server.Client().Do(req)
		if err != nil {
			fail <- err
		} else {
			done <- resp
		}
	}()
	require.NoError(t, mp.WriteField("purpose", "user_data"))
	part, err := mp.CreateFormFile("file", "revoked.txt")
	require.NoError(t, err)
	_, err = fmt.Fprint(part, "original bytes")
	require.NoError(t, err)
	// Wait for durable admission, then revoke while the multipart body stays open.
	require.Eventually(t, func() bool {
		var n int
		err := integrationDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM gateway_files WHERE user_id=$1 AND state='staging'`, f.Owner.UserID).Scan(&n)
		return err == nil && n == 1
	}, time.Second, 5*time.Millisecond)
	_, err = integrationDB.ExecContext(context.Background(), `UPDATE api_keys SET status='disabled' WHERE id=$1`, f.Owner.ID)
	require.NoError(t, err)
	require.NoError(t, mp.Close())
	require.NoError(t, writer.Close())
	select {
	case err := <-fail:
		require.NoError(t, err)
	case resp := <-done:
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 403, resp.StatusCode, string(body))
	case <-time.After(2 * time.Second):
		t.Fatal("upload failed to return after revocation")
	}
	var state string
	var charge int64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT state,reserved_bytes FROM gateway_files WHERE user_id=$1`, f.Owner.UserID).Scan(&state, &charge))
	require.Equal(t, "deleted", state)
	require.Zero(t, charge)
}

func TestFileLifecycleHTTPRevokedSubscriptionCannotPublishFromCache(t *testing.T) {
	for _, fault := range []bool{false, true} {
		t.Run(fmt.Sprint("database_failure=", fault), func(t *testing.T) {
			f := newLifecycleHTTPFixture(t)
			ctx := context.Background()
			client := testEntClient(t)
			_, err := client.Group.UpdateOneID(f.DBGroupID).SetSubscriptionType(service.SubscriptionTypeSubscription).Save(ctx)
			require.NoError(t, err)
			sub, err := client.UserSubscription.Create().SetUserID(f.Owner.UserID).SetGroupID(f.DBGroupID).SetStartsAt(time.Now().Add(-time.Hour)).SetExpiresAt(time.Now().Add(time.Hour)).SetStatus(service.SubscriptionStatusActive).Save(ctx)
			require.NoError(t, err)
			// Prime the real hot-path cache before revoking DB authorization.
			_, err = f.Subscriptions.GetActiveSubscription(ctx, f.Owner.UserID, f.DBGroupID)
			require.NoError(t, err)
			reader, writer := io.Pipe()
			defer writer.Close()
			mp := multipart.NewWriter(writer)
			req, err := http.NewRequest(http.MethodPost, f.Server.URL+"/v1/files", reader)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+f.Owner.Key)
			req.Header.Set("Content-Type", mp.FormDataContentType())
			done := make(chan *http.Response, 1)
			fail := make(chan error, 1)
			go func() {
				resp, err := f.Server.Client().Do(req)
				if err != nil {
					fail <- err
				} else {
					done <- resp
				}
			}()
			require.NoError(t, mp.WriteField("purpose", "user_data"))
			part, err := mp.CreateFormFile("file", "subscription.txt")
			require.NoError(t, err)
			_, err = fmt.Fprint(part, "original")
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				var n int
				err := integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_files WHERE user_id=$1 AND state='staging'`, f.Owner.UserID).Scan(&n)
				return err == nil && n == 1
			}, time.Second, 5*time.Millisecond)
			want := 403
			if fault {
				want = 503
				_, err = integrationDB.ExecContext(ctx, `ALTER TABLE user_subscriptions RENAME TO files_test_subscriptions_unavailable`)
				require.NoError(t, err)
				defer func() {
					_, err := integrationDB.ExecContext(context.Background(), `ALTER TABLE files_test_subscriptions_unavailable RENAME TO user_subscriptions`)
					require.NoError(t, err)
				}()
			} else {
				_, err = client.UserSubscription.UpdateOneID(sub.ID).SetStatus(service.SubscriptionStatusExpired).Save(ctx)
				require.NoError(t, err)
			}
			// No cache invalidation is delivered: publication must still query DB.
			require.NoError(t, mp.Close())
			require.NoError(t, writer.Close())
			select {
			case err := <-fail:
				require.NoError(t, err)
			case resp := <-done:
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, want, resp.StatusCode, string(body))
			case <-time.After(2 * time.Second):
				t.Fatal("subscription reauthorization did not return")
			}
			var charge int64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT reserved_bytes FROM gateway_files WHERE user_id=$1`, f.Owner.UserID).Scan(&charge))
			require.Zero(t, charge)
		})
	}
}
