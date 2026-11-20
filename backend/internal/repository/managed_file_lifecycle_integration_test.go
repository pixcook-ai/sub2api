//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// This adapter changes durations only. Every grant, renewal and release still
// uses the real PostgreSQL repository and the actual managed lifecycle port.
type managedLifecycleShortGrants struct{ service.FileRepository }

func (r managedLifecycleShortGrants) AcquireReadGrant(ctx context.Context, u, g int64, id, token string, _, _ time.Duration) (*service.StoredFile, service.FileReadGrant, error) {
	return r.FileRepository.AcquireReadGrant(ctx, u, g, id, token, 2*time.Second, 2*time.Second)
}

func (r managedLifecycleShortGrants) RenewReadGrant(ctx context.Context, id int64, token string, _, _ time.Duration) (service.FileReadGrant, error) {
	return r.FileRepository.RenewReadGrant(ctx, id, token, 2*time.Second, 2*time.Second)
}

type managedLifecycleObservedStore struct {
	service.FileBlobStore
	opened chan *os.File
	closed chan struct{}
}

func (s *managedLifecycleObservedStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	reader, err := s.FileBlobStore.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	file, ok := reader.(*os.File)
	if !ok {
		_ = reader.Close()
		return nil, fmt.Errorf("expected actual private-store file descriptor")
	}
	s.opened <- file
	return &managedLifecycleObservedReader{ReadCloser: reader, closed: s.closed}, nil
}

type managedLifecycleObservedReader struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (r *managedLifecycleObservedReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() { close(r.closed) })
	return err
}

func managedLifecycleFixture(t *testing.T, name string) (*lifecycleHTTPFixture, *ManagedFileLedgerRepository, service.FileRepository, service.FileOwner, *service.StoredFile) {
	t.Helper()
	f := newLifecycleHTTPFixture(t)
	id := f.upload(t, f.Owner, name, "user_data", nil)
	ledger := NewManagedFileRepository(integrationDB)
	repo := NewFileRepositoryWithRemoteLifecycle(integrationDB, ledger)
	owner := service.FileOwner{UserID: f.Owner.UserID, APIKeyID: f.Owner.ID, GroupID: f.DBGroupID}
	file, err := repo.GetForOwner(context.Background(), owner.UserID, owner.GroupID, id)
	require.NoError(t, err)
	return f, ledger, repo, owner, file
}

func TestManagedFileLifecycleRetirementDrainsActualReaderAndPreservesLateCopy(t *testing.T) {
	f, ledger, repo, owner, file := managedLifecycleFixture(t, "managed-drain.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	permit, err := ledger.Reserve(ctx, managedReserve(owner, file, 71, "combined-active-reader"))
	require.NoError(t, err)
	require.NoError(t, ledger.StartOperation(ctx, permit, file.PublicID))

	store := &managedLifecycleObservedStore{FileBlobStore: f.Store, opened: make(chan *os.File, 1), closed: make(chan struct{})}
	files, err := service.NewFileService(managedLifecycleShortGrants{repo}, store, f.Config.Gateway.Files)
	require.NoError(t, err)
	_, reader, err := files.Open(ctx, owner, file.PublicID)
	require.NoError(t, err)
	defer reader.Close()
	fd := <-store.opened
	prefix := make([]byte, 8)
	_, err = io.ReadFull(reader, prefix)
	require.NoError(t, err)
	require.Equal(t, "original", string(prefix), "the protected reader serves actual uploaded bytes")
	var readToken string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT token FROM gateway_file_read_leases WHERE file_id=$1`, file.ID).Scan(&readToken))

	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '100 milliseconds' WHERE id=$1`, file.ID)
	require.NoError(t, err)
	retired, err := repo.RetireExpired(ctx, file.ID, 2*time.Second)
	require.NoError(t, err)
	require.True(t, retired)
	var state, operationState string
	var fixed time.Time
	var version int64
	var lease, active sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state,read_drain_until,lifecycle_version FROM gateway_files WHERE id=$1`, file.ID).Scan(&state, &fixed, &version))
	require.Equal(t, "retired", state)
	require.Equal(t, file.Version+1, version)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state FROM gateway_file_remote_operations WHERE file_id=$1`, file.ID).Scan(&operationState))
	require.Equal(t, "uncertain", operationState)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT lease_token,active_operation_id FROM gateway_file_bindings WHERE file_id=$1`, file.ID).Scan(&lease, &active))
	require.False(t, lease.Valid)
	require.False(t, active.Valid)

	_, err = repo.ClaimLocalPurge(ctx, file.ID, "blocked-by-real-reader", time.Minute)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
	_, err = fd.Stat()
	require.NoError(t, err, "cleanup cannot unlink the protected private file while its grant is active")
	status, data := f.request(t, http.MethodGet, "/v1/files/"+file.PublicID+"/content", f.Rotated, nil, "")
	require.Equal(t, http.StatusNotFound, status, string(data))
	_, _, err = files.Open(ctx, owner, file.PublicID)
	require.ErrorIs(t, err, service.ErrFileNotFound)

	result := managedEnsureResult(permit)
	activated, err := ledger.CompleteOperation(ctx, permit, file.PublicID, result)
	require.NoError(t, err)
	require.False(t, activated, "a late confirmed copy cannot restore retired generation authority")
	require.ErrorIs(t, ledger.AuthorizeGeneration(ctx, permit), service.ErrFileNotFound)
	require.ErrorIs(t, ledger.StartOperation(ctx, permit, file.PublicID), service.ErrFileNotFound)
	before := managedLifecycleRemoteFacts(t, file.ID)

	// Do not call Read or Close: the service must close the actual FD and
	// release the token on its database grant/fixed drain deadline by itself.
	select {
	case <-store.closed:
	case <-time.After(4 * time.Second):
		t.Fatal("fixed drain watchdog did not close the actual private file")
	}
	_, err = fd.Stat()
	require.ErrorIs(t, err, os.ErrClosed)
	require.Eventually(t, func() bool {
		var n int
		err := integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_file_read_leases WHERE file_id=$1`, file.ID).Scan(&n)
		return err == nil && n == 0
	}, time.Second, 5*time.Millisecond)
	_, err = repo.RenewReadGrant(ctx, file.ID, readToken, 2*time.Second, 2*time.Second)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict, "drained or released tokens cannot resurrect")
	_, err = reader.Read(make([]byte, 1))
	require.Error(t, err)
	retired, err = repo.RetireExpired(ctx, file.ID, time.Hour)
	require.NoError(t, err)
	require.False(t, retired)
	var stillFixed time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT read_drain_until FROM gateway_files WHERE id=$1`, file.ID).Scan(&stillFixed))
	require.True(t, fixed.Equal(stillFixed), "a later worker cannot extend the fixed drain")

	ticket, err := repo.ClaimLocalPurge(ctx, file.ID, "confirmed-copy-local-purge", time.Minute)
	require.NoError(t, err)
	stale := *ticket
	stale.Version--
	require.ErrorIs(t, repo.ConfirmLocalPurge(ctx, stale), service.ErrFileLeaseConflict)
	require.NoError(t, f.Store.Remove(ctx, ticket.StorageKey))
	require.NoError(t, repo.ConfirmLocalPurge(ctx, *ticket))
	require.NoError(t, repo.ConfirmLocalPurge(ctx, *ticket), "same confirmed purge ticket is idempotent")
	require.Equal(t, before, managedLifecycleRemoteFacts(t, file.ID), "local quota release must retain every remote and identity fact")
	_, err = f.Store.Open(ctx, file.StorageKey)
	require.ErrorIs(t, err, os.ErrNotExist)
	var charge, bytes int64
	var possible, purged bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state,bytes,reserved_bytes,remote_activity_possible,local_purged_at IS NOT NULL FROM gateway_files WHERE id=$1`, file.ID).Scan(&state, &bytes, &charge, &possible, &purged))
	require.Equal(t, "retired", state)
	require.Equal(t, file.Bytes, bytes)
	require.Zero(t, charge)
	require.True(t, possible)
	require.True(t, purged)
}

func managedLifecycleRemoteFacts(t *testing.T, fileID int64) string {
	t.Helper()
	var facts string
	err := integrationDB.QueryRowContext(context.Background(), `SELECT jsonb_build_object(
 'binding',(SELECT to_jsonb(b) FROM gateway_file_bindings b WHERE b.file_id=$1),
 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM gateway_file_remote_operations o WHERE o.file_id=$1))::text`, fileID).Scan(&facts)
	require.NoError(t, err)
	return facts
}

func TestManagedFileLifecycleRetirementAndLedgerFenceRollbackTogether(t *testing.T) {
	_, ledger, repo, owner, file := managedLifecycleFixture(t, "managed-rollback.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	permit, err := ledger.Reserve(ctx, managedReserve(owner, file, 71, "combined-rollback"))
	require.NoError(t, err)
	require.NoError(t, ledger.StartOperation(ctx, permit, file.PublicID))
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, file.ID)
	require.NoError(t, err)
	before := managedLifecycleRemoteFacts(t, file.ID)
	trigger := fmt.Sprintf("managed_retirement_abort_%d", file.ID)
	_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic retirement commit failure'; END; $$;
 CREATE TRIGGER %s BEFORE UPDATE OF state ON gateway_files FOR EACH ROW WHEN (NEW.id=%d AND NEW.state='retired') EXECUTE FUNCTION %s()`, trigger, trigger, file.ID, trigger))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON gateway_files; DROP FUNCTION IF EXISTS %s()`, trigger, trigger))
		require.NoError(t, err)
	})
	retired, err := repo.RetireExpired(ctx, file.ID, time.Minute)
	require.Error(t, err)
	require.False(t, retired)
	require.Equal(t, before, managedLifecycleRemoteFacts(t, file.ID), "failed local retirement must roll back ledger lease/version/uncertain changes in the same transaction")
	var state string
	var version int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state,lifecycle_version FROM gateway_files WHERE id=$1`, file.ID).Scan(&state, &version))
	require.Equal(t, "ready", state)
	require.Equal(t, file.Version, version)
	_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER %s ON gateway_files; DROP FUNCTION %s()`, trigger, trigger))
	require.NoError(t, err)
	retired, err = repo.RetireExpired(ctx, file.ID, time.Minute)
	require.NoError(t, err)
	require.True(t, retired)
	activated, err := ledger.CompleteOperation(ctx, permit, file.PublicID, managedEnsureResult(permit))
	require.NoError(t, err)
	require.False(t, activated)
}

func TestManagedFileLifecycleConcurrentRetirementCannotReactivateLateCopy(t *testing.T) {
	_, ledger, repo, owner, file := managedLifecycleFixture(t, "managed-lock-race.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	permit, err := ledger.Reserve(ctx, managedReserve(owner, file, 71, "combined-retirement-race"))
	require.NoError(t, err)
	require.NoError(t, ledger.StartOperation(ctx, permit, file.PublicID))
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, file.ID)
	require.NoError(t, err)

	// Hold retirement after the real lifecycle port has fenced the ledger,
	// but before the file transition commits. A late completion must wait on
	// that same file lock and observe the committed retirement afterwards.
	gate, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	defer gate.Close()
	lockID := file.ID + 64000000
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID)
	require.NoError(t, err)
	defer gate.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)
	trigger := fmt.Sprintf("managed_retirement_wait_%d", file.ID)
	_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END; $$;
 CREATE TRIGGER %s BEFORE UPDATE OF state ON gateway_files FOR EACH ROW WHEN (NEW.id=%d AND NEW.state='retired') EXECUTE FUNCTION %s()`, trigger, lockID, trigger, file.ID, trigger))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON gateway_files; DROP FUNCTION IF EXISTS %s()`, trigger, trigger))
		require.NoError(t, err)
	})
	type outcome struct {
		active bool
		err    error
	}
	retirement := make(chan outcome, 1)
	go func() {
		retired, err := repo.RetireExpired(ctx, file.ID, time.Minute)
		retirement <- outcome{retired, err}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=$1::oid AND NOT granted)`, lockID).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 5*time.Millisecond, "actual retirement transaction must reach the gate")
	completed := make(chan outcome, 1)
	go func() {
		active, err := ledger.CompleteOperation(ctx, permit, file.PublicID, managedEnsureResult(permit))
		completed <- outcome{active, err}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%WHERE id=$1 AND user_id=$2 AND group_id=$3 FOR UPDATE%')`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 5*time.Millisecond, "late completion must wait on the actual locked file")
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID)
	require.NoError(t, err)
	r := <-retirement
	require.NoError(t, r.err)
	require.True(t, r.active)
	c := <-completed
	require.NoError(t, c.err)
	require.False(t, c.active)
	require.ErrorIs(t, ledger.AuthorizeGeneration(ctx, permit), service.ErrFileNotFound)
	var state, op, remote string
	var tokens int64
	var active sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT f.state,b.active_operation_id,o.state,o.remote_id,o.input_tokens
 FROM gateway_files f JOIN gateway_file_bindings b ON b.file_id=f.id JOIN gateway_file_remote_operations o ON o.file_id=f.id WHERE f.id=$1`, file.ID).Scan(&state, &active, &op, &remote, &tokens))
	require.Equal(t, "retired", state)
	require.False(t, active.Valid)
	require.Equal(t, "succeeded", op)
	require.Equal(t, "file-bps-confirmed", remote)
	require.EqualValues(t, 17, tokens)
}
