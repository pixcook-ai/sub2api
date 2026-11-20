//go:build integration

package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFileLifecycleUploadAdmissionKeepsSlotUntilCleanup(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	first := record("admitted", 100)
	require.NoError(t, repo.ReserveUpload(ctx, first, 1000, 1<<60, time.Minute, time.Minute, 1, 10000))
	rotated := record("rotated", 100)
	rotated.APIKeyID++
	rotated.GroupID++
	require.ErrorIs(t, repo.ReserveUpload(ctx, rotated, 1000, 1<<60, time.Minute, time.Minute, 1, 10000), service.ErrFileUploadConcurrency)
	ok, err := repo.AbortUpload(ctx, first.ID, first.UploadToken)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorIs(t, repo.ReserveUpload(ctx, rotated, 1000, 1<<60, time.Minute, time.Minute, 1, 10000), service.ErrFileUploadConcurrency)
	require.NoError(t, repo.FinishCleanup(ctx, first.ID, first.UploadToken))
	require.NoError(t, repo.ReserveUpload(ctx, rotated, 1000, 1<<60, time.Minute, time.Minute, 1, 10000))
}

func TestFileLifecycleUploadDeadlineCannotBeRenewedOrPublished(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	f := record("deadline", 100)
	require.NoError(t, repo.ReserveUpload(ctx, f, 1000, 1<<60, time.Minute, 10*time.Second, 1, 10000))
	var capped bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT upload_lease_until=upload_deadline_at FROM gateway_files WHERE id=$1`, f.ID).Scan(&capped))
	require.True(t, capped)
	require.NoError(t, repo.RenewUpload(ctx, f.ID, f.UploadToken, time.Minute))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT upload_lease_until=upload_deadline_at FROM gateway_files WHERE id=$1`, f.ID).Scan(&capped))
	require.True(t, capped)
	_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_files SET upload_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.ID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.RenewUpload(ctx, f.ID, f.UploadToken, time.Minute), service.ErrFileLeaseConflict)
	_, err = repo.CommitUploadMetadata(ctx, f.ID, f.UploadToken, 3, strings.Repeat("a", 64), "user_data", "final.txt", "text/plain", nil)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
}

func TestFileLifecycleGlobalUploadAdmissionIsAtomicAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	_, record := fileRepositoryFixture(t)
	var baseline int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_files WHERE state IN ('staging','tombstone')`).Scan(&baseline))
	files := []*service.StoredFile{record("first-process", 100), record("second-process", 100)}
	files[1].UserID++
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, `DELETE FROM gateway_files WHERE user_id=$1`, files[1].UserID)
		require.NoError(t, err)
	})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, f := range files {
		wg.Add(1)
		go func(f *service.StoredFile) {
			defer wg.Done()
			results <- NewFileRepository(integrationDB).ReserveUpload(ctx, f, 1000, 1<<60, time.Minute, time.Minute, 1, baseline+1)
		}(f)
	}
	wg.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, service.ErrFileUploadConcurrency)
			rejected++
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, 1, rejected)
}
