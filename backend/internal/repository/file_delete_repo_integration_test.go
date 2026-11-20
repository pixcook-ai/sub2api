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
	"github.com/stretchr/testify/require"
)

func TestFileDeleteLocalOnlyRetiresAndDrainsReaders(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("delete-local", 100)
	require.NoError(t, repo.Reserve(ctx, file, 1000, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	owner := service.FileOwner{UserID: file.UserID, GroupID: file.GroupID, APIKeyID: file.APIKeyID + 1}
	_, _, err := repo.AcquireReadGrant(ctx, owner.UserID, owner.GroupID, file.PublicID, "before-delete", time.Minute, time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute))
	_, err = repo.GetForOwner(ctx, owner.UserID, owner.GroupID, file.PublicID)
	require.ErrorIs(t, err, service.ErrFileNotFound)
	_, err = repo.AcquireRead(ctx, owner.UserID, owner.GroupID, file.PublicID, "after-delete", time.Minute)
	require.ErrorIs(t, err, service.ErrFileNotFound)
	_, err = repo.RenewReadGrant(ctx, file.ID, "before-delete", time.Minute, time.Minute)
	require.NoError(t, err, "existing readers drain under the fixed retirement cap")
	_, err = repo.ClaimLocalPurge(ctx, file.ID, "purge", time.Minute)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
	require.NoError(t, repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute), "owner retries are idempotent")
	require.NoError(t, repo.ReleaseRead(ctx, file.ID, "before-delete"))
	ticket, err := repo.ClaimLocalPurge(ctx, file.ID, "purge", time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.ConfirmLocalPurge(ctx, *ticket))
	require.NoError(t, repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute), "purged tombstone remains idempotent")
}

func TestFileDeleteLocalOnlyRejectsRemoteAndReservedEvidence(t *testing.T) {
	for _, state := range []string{"flag-only", "reserved", "started", "uncertain", "succeeded", "released"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			ledger, owner, file := managedLedgerFixture(t)
			repo := &fileRepository{db: integrationDB}
			var permit *service.ManagedFileReservation
			var err error
			if state == "flag-only" {
				_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET remote_activity_possible=true WHERE id=$1`, file.ID)
				require.NoError(t, err)
			} else {
				permit, err = ledger.Reserve(ctx, managedReserve(owner, file, 71, "delete-remote-guard"))
				require.NoError(t, err)
				if state != "reserved" && state != "released" {
					require.NoError(t, ledger.StartOperation(ctx, permit, file.PublicID))
				}
				switch state {
				case "uncertain":
					require.NoError(t, ledger.MarkUncertain(ctx, permit, file.PublicID, "upload_outcome_unknown"))
				case "succeeded":
					active, err := ledger.CompleteOperation(ctx, permit, file.PublicID, managedEnsureResult(permit))
					require.NoError(t, err)
					require.True(t, active)
				case "released":
					require.NoError(t, ledger.ReleaseReservation(ctx, permit))
				}
			}
			before, err := repo.GetForOwner(ctx, owner.UserID, owner.GroupID, file.PublicID)
			require.NoError(t, err)
			require.ErrorIs(t, repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute), service.ErrFileRemoteDeleteUnsupported)
			after, err := repo.GetForOwner(ctx, owner.UserID, owner.GroupID, file.PublicID)
			require.NoError(t, err)
			require.Equal(t, before.Version, after.Version, "unsupported deletion must not retire or alter the file")
			if state == "started" {
				active, err := ledger.CompleteOperation(ctx, permit, file.PublicID, managedEnsureResult(permit))
				require.NoError(t, err, "a late confirmation remains durable")
				require.True(t, active, "a rejected deletion does not change the existing generation permit")
			}
		})
	}
}

func TestFileDeleteLocalOnlyPreservesOwnerAndExpiryBoundaries(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("delete-owner", 100)
	require.NoError(t, repo.Reserve(ctx, file, 1000, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	owner := service.FileOwner{UserID: file.UserID, GroupID: file.GroupID, APIKeyID: file.APIKeyID}
	for _, scope := range []service.FileOwner{
		{UserID: owner.UserID + 1, GroupID: owner.GroupID, APIKeyID: owner.APIKeyID},
		{UserID: owner.UserID, GroupID: owner.GroupID + 1, APIKeyID: owner.APIKeyID},
	} {
		require.ErrorIs(t, repo.DeleteLocalFile(ctx, scope, file.PublicID, time.Minute), service.ErrFileNotFound)
	}
	require.ErrorIs(t, repo.DeleteLocalFile(ctx, owner, "file-unknown", time.Minute), service.ErrFileNotFound)
	_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, file.ID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute), service.ErrFileNotFound)
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=NULL WHERE id=$1`, file.ID)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute))
	for _, scope := range []service.FileOwner{
		{UserID: owner.UserID + 1, GroupID: owner.GroupID, APIKeyID: owner.APIKeyID},
		{UserID: owner.UserID, GroupID: owner.GroupID + 1, APIKeyID: owner.APIKeyID},
	} {
		require.ErrorIs(t, repo.DeleteLocalFile(ctx, scope, file.PublicID, time.Minute), service.ErrFileNotFound, "tombstone existence is still owner scoped")
	}
}

func TestFileDeleteLocalOnlyRacesManagedReservation(t *testing.T) {
	for i := 0; i < 6; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ctx := context.Background()
			ledger, owner, file := managedLedgerFixture(t)
			repo := &fileRepository{db: integrationDB}
			start := make(chan struct{})
			var wg sync.WaitGroup
			var deleteErr, reserveErr error
			var permit *service.ManagedFileReservation
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				deleteErr = repo.DeleteLocalFile(ctx, owner, file.PublicID, time.Minute)
			}()
			go func() {
				defer wg.Done()
				<-start
				permit, reserveErr = ledger.Reserve(ctx, managedReserve(owner, file, 71, "delete-race"))
			}()
			close(start)
			wg.Wait()
			switch {
			case deleteErr == nil:
				require.ErrorIs(t, reserveErr, service.ErrFileNotFound)
			case reserveErr == nil:
				require.ErrorIs(t, deleteErr, service.ErrFileRemoteDeleteUnsupported)
				require.NotNil(t, permit)
			default:
				t.Fatalf("both operations failed: delete=%v reserve=%v", deleteErr, reserveErr)
			}
			require.False(t, deleteErr == nil && reserveErr == nil, "deletion and a usable upload permit cannot both win")
		})
	}
}

func TestFileDeleteLocalOnlyCancellationDoesNotRetire(t *testing.T) {
	ctx := context.Background()
	repo, record := fileRepositoryFixture(t)
	file := record("delete-canceled", 100)
	require.NoError(t, repo.Reserve(ctx, file, 1000, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	owner := service.FileOwner{UserID: file.UserID, GroupID: file.GroupID, APIKeyID: file.APIKeyID}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.True(t, errors.Is(repo.DeleteLocalFile(canceled, owner, file.PublicID, time.Minute), context.Canceled))
	_, err := repo.GetForOwner(ctx, owner.UserID, owner.GroupID, file.PublicID)
	require.NoError(t, err)
}
