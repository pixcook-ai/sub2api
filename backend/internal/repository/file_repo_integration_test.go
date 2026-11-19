//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func fileRepositoryFixture(t *testing.T) (*fileRepository, func(string, int64) *service.StoredFile) {
	t.Helper()
	uid := time.Now().UnixNano()
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), `DELETE FROM gateway_files WHERE user_id=$1`, uid)
		require.NoError(t, err)
	})
	seq := 0
	return &fileRepository{db: integrationDB}, func(label string, size int64) *service.StoredFile {
		seq++
		return &service.StoredFile{
			PublicID: fmt.Sprintf("file-%d-%s-%d", uid, label, seq), UserID: uid, APIKeyID: 101, GroupID: 42,
			StorageKey: fmt.Sprintf("%016x%016x", uid, seq), Filename: "original.txt", MIME: "text/plain",
			Purpose: "user_data", ReservedBytes: size, UploadToken: fmt.Sprintf("upload-%d-%d", uid, seq),
		}
	}
}

func TestFileRepositoryDurableOwnershipAndReadLeases(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("ownership", 100)
	require.NoError(t, repo.Reserve(ctx, file, 1000, 1<<60, time.Minute))
	_, err := repo.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.ErrorIs(t, err, service.ErrFileNotFound, "staging resources are never public")
	require.ErrorIs(t, repo.CommitUpload(ctx, file.ID, "not-owner", 3, strings.Repeat("a", 64), "user_data"), service.ErrFileLeaseConflict)
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))

	// A new repository/process and rotated key still resolve by user + group.
	otherProcess := NewFileRepository(integrationDB)
	got, err := otherProcess.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.NoError(t, err)
	require.Equal(t, int64(101), got.APIKeyID, "key remains audit provenance")
	require.Equal(t, int64(3), got.ReservedBytes)
	for _, scope := range [][2]int64{{file.UserID + 1, file.GroupID}, {file.UserID, file.GroupID + 1}} {
		_, err = otherProcess.GetForOwner(ctx, scope[0], scope[1], file.PublicID)
		require.ErrorIs(t, err, service.ErrFileNotFound)
	}
	_, err = repo.AcquireRead(ctx, file.UserID, file.GroupID, file.PublicID, "reader-one", time.Minute)
	require.NoError(t, err)
	_, err = otherProcess.AcquireRead(ctx, file.UserID, file.GroupID, file.PublicID, "reader-two", time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.RenewRead(ctx, file.ID, "reader-one", time.Minute))
	require.ErrorIs(t, repo.RenewRead(ctx, file.ID, "reader-one", 0), service.ErrFileLeaseConflict,
		"invalid renewal must not revoke a valid reader's existing lease")
	require.NoError(t, repo.RenewRead(ctx, file.ID, "reader-one", time.Minute))
	require.ErrorIs(t, repo.RenewRead(ctx, file.ID, "wrong-reader", time.Minute), service.ErrFileLeaseConflict)
	require.NoError(t, repo.ReleaseRead(ctx, file.ID, "reader-one"))
	var readers int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_file_read_leases WHERE file_id=$1`, file.ID).Scan(&readers))
	require.Equal(t, 1, readers, "one process cannot release another process' read lease")
	require.NoError(t, repo.ReleaseRead(ctx, file.ID, "reader-two"))
	aborted, err := repo.AbortUpload(ctx, file.ID, file.UploadToken)
	require.NoError(t, err)
	require.False(t, aborted, "completed file cannot be erased by a late upload cleanup")
}

func TestFileRepositoryCrossProcessQuotaReservation(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	files := []*service.StoredFile{record("budget-a", 100), record("budget-b", 100)}
	var baseline int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_bytes),0) FROM gateway_files WHERE state<>'deleted'`).Scan(&baseline))
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, file := range files {
		wg.Add(1)
		go func(file *service.StoredFile) {
			defer wg.Done()
			results <- NewFileRepository(integrationDB).Reserve(ctx, file, 1000, baseline+100, time.Minute)
		}(file)
	}
	wg.Wait()
	close(results)
	succeeded, rejected := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, service.ErrFileQuotaExceeded) {
			rejected++
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, rejected, "independent process reservations share a single durable global budget")
	var winner *service.StoredFile
	for _, file := range files {
		if file.ID != 0 {
			winner = file
		}
	}
	require.NotNil(t, winner)
	ok, err := repo.AbortUpload(ctx, winner.ID, winner.UploadToken)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorIs(t, repo.Reserve(ctx, record("not-cleaned", 100), 100, 1<<60, time.Minute), service.ErrFileQuotaExceeded,
		"tombstone retains user budget until bytes were actually removed")
	require.NoError(t, repo.FinishCleanup(ctx, winner.ID, winner.UploadToken))
	require.NoError(t, repo.Reserve(ctx, record("after-cleanup", 100), 100, 1<<60, time.Minute))
}

func TestFileRepositoryStaleUploadCASRecovery(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("stale", 100)
	require.NoError(t, repo.Reserve(ctx, file, 100, 1<<60, time.Minute))
	_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_files SET upload_lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, file.ID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"), service.ErrFileLeaseConflict)
	require.ErrorIs(t, repo.RenewUpload(ctx, file.ID, file.UploadToken, time.Minute), service.ErrFileLeaseConflict, "expired uploader cannot resurrect itself")
	claimed, err := repo.ClaimStaleUploads(ctx, "recovery-owner", time.Minute, 100)
	require.NoError(t, err)
	var recovered *service.StoredFile
	for _, got := range claimed {
		if got.ID == file.ID {
			recovered = got
		}
	}
	require.NotNil(t, recovered)
	require.Equal(t, "tombstone", recovered.State)
	require.Equal(t, file.StorageKey, recovered.StorageKey, "rename-before-DB-commit crash retains the exact blob cleanup key")
	oldAborted, err := repo.AbortUpload(ctx, file.ID, file.UploadToken)
	require.NoError(t, err)
	require.False(t, oldAborted)
	require.ErrorIs(t, repo.FinishCleanup(ctx, file.ID, file.UploadToken), service.ErrFileLeaseConflict)
	claimedAgain, err := repo.ClaimStaleUploads(ctx, "second-recovery", time.Minute, 100)
	require.NoError(t, err)
	for _, got := range claimedAgain {
		require.NotEqual(t, file.ID, got.ID, "live recovery lease cannot be stolen")
	}
	require.NoError(t, repo.FinishCleanup(ctx, file.ID, "recovery-owner"))
	_, err = repo.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.ErrorIs(t, err, service.ErrFileNotFound)
	require.NoError(t, repo.Reserve(ctx, record("reclaimed-budget", 100), 100, 1<<60, time.Minute))
}

func TestFileRepositoryReadRenewalSerializesWithFileMutation(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("read-lock", 100)
	require.NoError(t, repo.Reserve(ctx, file, 100, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	_, err := repo.AcquireRead(ctx, file.UserID, file.GroupID, file.PublicID, "reader", time.Minute)
	require.NoError(t, err)

	// Future ready->tombstone transitions must lock this same file row before
	// inspecting read leases. A renewal may not race past that decision.
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SELECT id FROM gateway_files WHERE id=$1 FOR UPDATE`, file.ID)
	require.NoError(t, err)
	deadlineCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	err = NewFileRepository(integrationDB).RenewRead(deadlineCtx, file.ID, "reader", time.Minute)
	require.Error(t, err, "renewal must wait for an in-flight file mutation rather than observe stale ready state")
	require.ErrorIs(t, deadlineCtx.Err(), context.DeadlineExceeded)
	require.NoError(t, tx.Rollback())
	require.NoError(t, repo.RenewRead(ctx, file.ID, "reader", time.Minute), "lock timeout leaves the original lease intact")
}

func TestFileRepositoryMigrationRerunPreservesReadyFilesAndConstraints(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("migration", 100)
	require.NoError(t, repo.Reserve(ctx, file, 100, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	_, err := repo.AcquireRead(ctx, file.UserID, file.GroupID, file.PublicID, "migration-reader", time.Minute)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, integrationDB), "startup migration rerun is non-destructive")
	got, err := repo.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.NoError(t, err)
	require.Equal(t, file.ID, got.ID)
	require.Equal(t, strings.Repeat("a", 64), got.SHA256)
	var readers int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_file_read_leases WHERE file_id=$1`, file.ID).Scan(&readers))
	require.Equal(t, 1, readers)
	for _, statement := range []string{
		`UPDATE gateway_files SET state='unknown' WHERE id=$1`,
		`UPDATE gateway_files SET state='staging',upload_token=NULL,upload_lease_until=NULL WHERE id=$1`,
		`UPDATE gateway_files SET reserved_bytes=2 WHERE id=$1`,
	} {
		_, err = integrationDB.ExecContext(ctx, statement, file.ID)
		var pgErr *pq.Error
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, pq.ErrorCode("23514"), pgErr.Code, "real PostgreSQL check constraints reject corrupt lifecycle/budget writes")
	}
	got, err = repo.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.NoError(t, err)
	require.Equal(t, "ready", got.State)
}
