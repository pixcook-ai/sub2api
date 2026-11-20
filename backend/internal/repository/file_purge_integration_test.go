//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type lifecycleRemoteGuard struct{ err error }

func (g lifecycleRemoteGuard) FenceForRetirement(context.Context, *sql.Tx, service.FileLifecycleSnapshot) (service.FileRemoteDisposition, error) {
	return service.FileRemoteDisposition{Kind: service.FileRemoteUncertain}, g.err
}
func (g lifecycleRemoteGuard) InspectForLocalPurge(context.Context, *sql.Tx, service.FileLifecycleSnapshot) (service.FileRemoteDisposition, error) {
	return service.FileRemoteDisposition{Kind: service.FileRemoteUncertain}, g.err
}

func TestFileLifecyclePurgePreservesActiveReaderAndRemoteEvidence(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	ctx := context.Background()
	id := f.upload(t, f.Owner, "original.txt", "user_data", nil)
	repo := NewFileRepository(integrationDB)
	file, reader, err := f.Files.Open(ctx, service.FileOwner{UserID: f.Owner.UserID, GroupID: f.DBGroupID}, id)
	require.NoError(t, err)
	defer reader.Close()
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, file.ID)
	require.NoError(t, err)
	retired, err := repo.RetireExpired(ctx, file.ID, time.Minute*15)
	require.NoError(t, err)
	require.True(t, retired)
	_, err = repo.ClaimLocalPurge(ctx, file.ID, "first", time.Minute)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
	require.NoError(t, reader.Close())
	ticket, err := repo.ClaimLocalPurge(ctx, file.ID, "second", time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.Store.Remove(ctx, ticket.StorageKey))
	require.NoError(t, repo.ConfirmLocalPurge(ctx, *ticket))
	require.NoError(t, repo.ConfirmLocalPurge(ctx, *ticket), "same confirmed ticket is idempotent")
	var state string
	var bytes, charge int64
	var purged sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state,bytes,reserved_bytes,local_purged_at FROM gateway_files WHERE id=$1`, file.ID).Scan(&state, &bytes, &charge, &purged))
	require.Equal(t, "retired", state)
	require.Positive(t, bytes)
	require.Zero(t, charge)
	require.True(t, purged.Valid)
	_, err = f.Store.Open(ctx, file.StorageKey)
	require.ErrorIs(t, err, os.ErrNotExist)

	remoteID := f.upload(t, f.Owner, "remote.txt", "user_data", nil)
	var remoteRow int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second',remote_activity_possible=true WHERE public_id=$1 RETURNING id`, remoteID).Scan(&remoteRow))
	_, err = repo.RetireExpired(ctx, remoteRow, 15*time.Minute)
	require.ErrorIs(t, err, service.ErrFileRemoteGuardUnavailable)
	guarded := NewFileRepositoryWithRemoteLifecycle(integrationDB, lifecycleRemoteGuard{})
	retired, err = guarded.RetireExpired(ctx, remoteRow, 15*time.Minute)
	require.NoError(t, err)
	require.True(t, retired)
	_, err = repo.ClaimLocalPurge(ctx, remoteRow, "missing-port", time.Minute)
	require.ErrorIs(t, err, service.ErrFileRemoteGuardUnavailable)
	failed := NewFileRepositoryWithRemoteLifecycle(integrationDB, lifecycleRemoteGuard{err: errors.New("ledger unavailable")})
	_, err = failed.ClaimLocalPurge(ctx, remoteRow, "failed-port", time.Minute)
	require.Error(t, err)
	ticket, err = guarded.ClaimLocalPurge(ctx, remoteRow, "actual-port", time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.Store.Remove(ctx, ticket.StorageKey))
	require.NoError(t, guarded.ConfirmLocalPurge(ctx, *ticket))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state,bytes,reserved_bytes FROM gateway_files WHERE id=$1`, remoteRow).Scan(&state, &bytes, &charge))
	require.Equal(t, "retired", state)
	require.Positive(t, bytes)
	require.Zero(t, charge)
}

func TestFileLifecycleWorkerRestartAndUnlinkBeforeConfirmRecover(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	ctx := context.Background()
	id := f.upload(t, f.Owner, "restart.txt", "user_data", nil)
	var row int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE public_id=$1 RETURNING id`, id).Scan(&row))
	repo := NewFileRepository(integrationDB)
	_, err := repo.RetireExpired(ctx, row, 15*time.Minute)
	require.NoError(t, err)
	ticket, err := repo.ClaimLocalPurge(ctx, row, "crashed-worker", time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.Store.Remove(ctx, ticket.StorageKey))
	// Crash after unlink, before confirming quota release. A new process must
	// retain charge while the old cleanup claim is live, then safely retry.
	require.NoError(t, f.Files.RunLifecycleOnce(ctx))
	var charge int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT reserved_bytes FROM gateway_files WHERE id=$1`, row).Scan(&charge))
	require.Positive(t, charge)
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET cleanup_lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, row)
	require.NoError(t, err)
	restarted, err := service.NewFileService(repo, f.Store, f.Config.Gateway.Files)
	require.NoError(t, err)
	require.NoError(t, restarted.RunLifecycleOnce(ctx))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT reserved_bytes FROM gateway_files WHERE id=$1`, row).Scan(&charge))
	require.Zero(t, charge)
	require.ErrorIs(t, repo.ConfirmLocalPurge(ctx, *ticket), service.ErrFileLeaseConflict, "old cleanup owner cannot confirm a taken-over claim")
}

func TestFileLifecycleReaderCancellationReleasesWithoutFurtherReads(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	id := f.upload(t, f.Owner, "cancel-reader.txt", "user_data", nil)
	file, reader, err := f.Files.Open(ctx, service.FileOwner{UserID: f.Owner.UserID, GroupID: f.DBGroupID}, id)
	require.NoError(t, err)
	defer reader.Close()
	cancel()
	require.Eventually(t, func() bool {
		var n int
		err := integrationDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM gateway_file_read_leases WHERE file_id=$1`, file.ID).Scan(&n)
		return err == nil && n == 0
	}, time.Second, 5*time.Millisecond, "cancel must close and release even without Read")
	_, err = reader.Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, reader.Close())
}

// Shorten only the real DB lease at the repository boundary, while production
// config retains its minimum. The blocked renewal simulates a stalled DB call.
type stalledGrantRepository struct{ service.FileRepository }

func (r stalledGrantRepository) AcquireReadGrant(ctx context.Context, u, g int64, id, token string, lease, drain time.Duration) (*service.StoredFile, service.FileReadGrant, error) {
	return r.FileRepository.AcquireReadGrant(ctx, u, g, id, token, 150*time.Millisecond, drain)
}
func (r stalledGrantRepository) RenewReadGrant(ctx context.Context, _ int64, _ string, _, _ time.Duration) (service.FileReadGrant, error) {
	<-ctx.Done()
	return service.FileReadGrant{}, ctx.Err()
}

func TestFileLifecycleReaderGrantExpiresWithoutReadWhileRenewalStalls(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	id := f.upload(t, f.Owner, "stalled-reader.txt", "user_data", nil)
	svc, err := service.NewFileService(stalledGrantRepository{NewFileRepository(integrationDB)}, f.Store, f.Config.Gateway.Files)
	require.NoError(t, err)
	found, reader, err := svc.Open(context.Background(), service.FileOwner{UserID: f.Owner.UserID, GroupID: f.DBGroupID}, id)
	require.NoError(t, err)
	defer reader.Close()
	require.Eventually(t, func() bool {
		var n int
		err := integrationDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM gateway_file_read_leases WHERE file_id=$1`, found.ID).Scan(&n)
		return err == nil && n == 0
	}, time.Second, 5*time.Millisecond, "watchdog must close and release at actual DB grant")
	_, err = reader.Read(make([]byte, 1))
	require.Error(t, err)
	require.NoError(t, reader.Close())
}

func TestFileLifecycleWorkerAdvancesPastBlockedRemoteFile(t *testing.T) {
	f := newLifecycleHTTPFixture(t)
	blocked := f.upload(t, f.Owner, "blocked.txt", "user_data", nil)
	later := f.upload(t, f.Owner, "later.txt", "user_data", nil)
	_, err := integrationDB.ExecContext(context.Background(), `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second',remote_activity_possible=(public_id=$1) WHERE public_id IN ($1,$2)`, blocked, later)
	require.NoError(t, err)
	cfg := f.Config.Gateway.Files
	cfg.LifecycleWorkerBatchSize = 1
	svc, err := service.NewFileService(NewFileRepository(integrationDB), f.Store, cfg)
	require.NoError(t, err)
	var purged bool
	for i := 0; i < 100 && !purged; i++ {
		_ = svc.RunLifecycleOnce(context.Background())
		require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT local_purged_at IS NOT NULL FROM gateway_files WHERE public_id=$1`, later).Scan(&purged))
	}
	require.True(t, purged, "blocked earlier row must not starve later expired files")
	var state string
	var charge int64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT state,reserved_bytes FROM gateway_files WHERE public_id=$1`, blocked).Scan(&state, &charge))
	require.Equal(t, "ready", state)
	require.Positive(t, charge)
	svc.StartLifecycle()
	svc.StopLifecycle()
	svc.StopLifecycle()
	svc.StartLifecycle()
	svc.StopLifecycle()
}
