//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestFileLifecycleRepositoryListStableOwnerScopedCursor(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	stamp := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	create := func(label, purpose string) *service.StoredFile {
		file := record(label, 100)
		require.NoError(t, repo.Reserve(ctx, file, 1<<30, 1<<60, time.Minute))
		require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 1, strings.Repeat("a", 64), purpose))
		_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_files SET created_at=$2 WHERE id=$1`, file.ID, stamp)
		require.NoError(t, err)
		return file
	}
	files := []*service.StoredFile{create("one", "user_data"), create("two", "user_data"), create("three", "user_data"), create("four", "user_data"), create("five", "user_data")}
	owner := service.FileOwner{UserID: files[0].UserID, GroupID: files[0].GroupID, APIKeyID: 999999}
	otherPurpose := create("purpose", "assistants")
	otherGroup := create("group", "user_data")
	_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_files SET group_id=group_id+1 WHERE id=$1`, otherGroup.ID)
	require.NoError(t, err)
	expired := create("expired", "user_data")
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=statement_timestamp()-INTERVAL '1 second' WHERE id=$1`, expired.ID)
	require.NoError(t, err)
	foreignRepo, foreignRecord := fileRepositoryFixture(t)
	foreign := foreignRecord("foreign", 100)
	require.NoError(t, foreignRepo.Reserve(ctx, foreign, 1<<30, 1<<60, time.Minute))
	require.NoError(t, foreignRepo.CommitUpload(ctx, foreign.ID, foreign.UploadToken, 1, strings.Repeat("b", 64), "user_data"))

	list := func(after, order string, limit int) service.FileListPage {
		t.Helper()
		page, err := repo.ListVisibleFiles(ctx, owner, service.FileListQuery{After: after, Order: order, Limit: limit, Purpose: "user_data"})
		require.NoError(t, err)
		return page
	}
	ids := func(page service.FileListPage) []string {
		out := make([]string, len(page.Files))
		for i, file := range page.Files {
			out[i] = file.PublicID
		}
		return out
	}
	page := list("", "asc", 2)
	require.Equal(t, []string{files[0].PublicID, files[1].PublicID}, ids(page), "equal timestamps use the immutable ID tie breaker")
	require.True(t, page.HasMore)
	require.Equal(t, int64(1), page.Files[0].Version)
	page = list(files[1].PublicID, "asc", 2)
	require.Equal(t, []string{files[2].PublicID, files[3].PublicID}, ids(page))
	require.True(t, page.HasMore)
	page = list(files[3].PublicID, "asc", 2)
	require.Equal(t, []string{files[4].PublicID}, ids(page))
	require.False(t, page.HasMore, "other users, groups, purposes and expired rows do not affect has_more")
	page = list("", "desc", 2)
	require.Equal(t, []string{files[4].PublicID, files[3].PublicID}, ids(page))
	require.True(t, page.HasMore)
	page = list(files[3].PublicID, "desc", 2)
	require.Equal(t, []string{files[2].PublicID, files[1].PublicID}, ids(page))
	require.True(t, page.HasMore)

	// Retiring or deleting a cursor must not strand the remaining page.
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET state='retired',retired_at=statement_timestamp(),
 retire_reason='deleted',read_drain_until=statement_timestamp()+INTERVAL '1 minute',lifecycle_version=lifecycle_version+1 WHERE id=$1`, files[1].ID)
	require.NoError(t, err)
	require.Equal(t, []string{files[2].PublicID, files[3].PublicID}, ids(list(files[1].PublicID, "asc", 2)))
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET state='deleted',reserved_bytes=0,local_purged_at=statement_timestamp() WHERE id=$1`, files[1].ID)
	require.NoError(t, err)
	require.Equal(t, []string{files[2].PublicID, files[3].PublicID}, ids(list(files[1].PublicID, "asc", 2)))
	page = list(expired.PublicID, "asc", 2)
	require.Empty(t, page.Files, "an owned expired cursor remains valid")
	require.False(t, page.HasMore)

	for _, after := range []string{foreign.PublicID, otherGroup.PublicID, otherPurpose.PublicID, "file-not-known"} {
		_, err = repo.ListVisibleFiles(ctx, owner, service.FileListQuery{After: after, Order: "asc", Limit: 2, Purpose: "user_data"})
		require.ErrorIs(t, err, service.ErrFileInvalidCursor)
	}
	_, err = repo.ListVisibleFiles(ctx, owner, service.FileListQuery{Order: "asc; DROP TABLE gateway_files", Limit: 2})
	require.ErrorIs(t, err, service.ErrInvalidFile)
}

func TestFileLifecycleMigrationPreservesMetadataAndMonotonicRemoteEvidence(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("lifecycle-schema", 100)
	require.NoError(t, repo.Reserve(ctx, file, 100, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=statement_timestamp()+INTERVAL '1 hour',remote_activity_possible=TRUE WHERE id=$1`, file.ID)
	require.NoError(t, err)
	got, err := repo.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	require.True(t, got.RemoteActivityPossible)
	require.Equal(t, int64(1), got.Version)
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET remote_activity_possible=FALSE WHERE id=$1`, file.ID)
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23514"), pgErr.Code)
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET state='retired',retired_at=statement_timestamp(),
 retire_reason='deleted',read_drain_until=statement_timestamp()+INTERVAL '1 minute',local_purged_at=statement_timestamp(),
 reserved_bytes=0,lifecycle_version=lifecycle_version+1 WHERE id=$1`, file.ID)
	require.NoError(t, err, "local charge can be released while immutable remote-relevant metadata remains")
	got, err = scanStoredFile(integrationDB.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files WHERE id=$1`, file.ID))
	require.NoError(t, err)
	require.Equal(t, int64(3), got.Bytes)
	require.Zero(t, got.ReservedBytes)
	require.NotNil(t, got.LocalPurgedAt)
	require.NotNil(t, got.RetiredAt)
	require.NotNil(t, got.ReadDrainUntil)
	require.Equal(t, int64(2), got.Version)
	require.True(t, got.RemoteActivityPossible)
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET local_purged_at=NULL WHERE id=$1`, file.ID)
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23514"), pgErr.Code)
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET cleanup_token='partial-lease' WHERE id=$1`, file.ID)
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23514"), pgErr.Code)
	claimed, err := repo.ClaimStaleUploads(ctx, "old-staging-recovery", time.Minute, 100)
	require.NoError(t, err)
	for _, row := range claimed {
		require.NotEqual(t, file.ID, row.ID, "retired files never enter 06 staging recovery")
	}
	require.ErrorIs(t, repo.FinishCleanup(ctx, file.ID, "old-staging-recovery"), service.ErrFileLeaseConflict)
	require.NoError(t, ApplyMigrations(ctx, integrationDB))
}
