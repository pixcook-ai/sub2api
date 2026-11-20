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

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func managedLedgerFixture(t *testing.T) (*ManagedFileLedgerRepository, service.FileOwner, *service.StoredFile) {
	t.Helper()
	repo, record := fileRepositoryFixture(t)
	file := record("managed", 100)
	ctx := context.Background()
	require.NoError(t, repo.Reserve(ctx, file, 1000, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(ctx, file.ID, file.UploadToken, 3, strings.Repeat("a", 64), "user_data"))
	file, err := repo.GetForOwner(ctx, file.UserID, file.GroupID, file.PublicID)
	require.NoError(t, err)
	// Remote evidence is durable in production; isolated fixtures remove their
	// ledger explicitly before the base fixture removes its synthetic file.
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, `DELETE FROM gateway_file_remote_operations WHERE file_id=$1`, file.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `DELETE FROM gateway_file_bindings WHERE file_id=$1`, file.ID)
		require.NoError(t, err)
	})
	return NewManagedFileRepository(integrationDB), service.FileOwner{UserID: file.UserID, APIKeyID: file.APIKeyID, GroupID: file.GroupID}, file
}

func managedReserve(owner service.FileOwner, file *service.StoredFile, account int64, token string) service.ManagedFileReserveInput {
	return service.ManagedFileReserveInput{
		Owner: owner, Files: []service.ManagedFileVersion{{PublicID: file.PublicID, Version: file.Version}},
		Selection:  service.ManagedFileSelection{AccountID: account, Identity: managedfiles.Identity{BackendID: "backend-test", SubjectFingerprint: "sha256:" + strings.Repeat("b", 64), CredentialGeneration: "epoch-1"}},
		LeaseToken: token, Lease: time.Minute,
	}
}

func TestManagedFileLedgerConcurrentFirstUseFreezesOneAccount(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, token := range []string{"leader-a", "leader-b"} {
		wg.Add(1)
		go func(account int64, token string) {
			defer wg.Done()
			_, err := NewManagedFileRepository(integrationDB).Reserve(ctx, managedReserve(owner, file, account, token))
			results <- err
		}(int64(71+i), token)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, service.ErrManagedFileBindingConflict) {
			conflict++
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflict)
	lookup, err := repo.LookupBindings(ctx, owner, []string{file.PublicID})
	require.NoError(t, err)
	require.Contains(t, []int64{71, 72}, lookup.AccountID)
	require.Len(t, lookup.Bindings, 1)
	require.NotEmpty(t, lookup.Bindings[0].Operation.ID)
	require.Equal(t, "reserved", lookup.Bindings[0].Operation.State)
}

func TestManagedFileLedgerStartsBeforeRemoteAndPreservesLateSuccess(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	permit, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "upload-leader"))
	require.NoError(t, err)
	require.NoError(t, repo.StartOperation(ctx, permit, file.PublicID))
	var remotePossible bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT remote_activity_possible FROM gateway_files WHERE id=$1`, file.ID).Scan(&remotePossible))
	require.True(t, remotePossible, "a committed started operation must already fence local-only cleanup")
	require.ErrorIs(t, repo.StartOperation(ctx, permit, file.PublicID), service.ErrManagedFileUncertain,
		"the same operation may not send a second upload after started")
	require.NoError(t, repo.MarkUncertain(ctx, permit, file.PublicID, "upstream_timeout"))
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_file_bindings SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE file_id=$1`, file.ID)
	require.NoError(t, err)
	result := managedEnsureResult(permit)
	activated, err := repo.CompleteOperation(ctx, permit, file.PublicID, result)
	require.NoError(t, err)
	require.False(t, activated, "late success is permanent evidence, not fresh generation authority")
	lookup, err := repo.LookupBindings(ctx, owner, []string{file.PublicID})
	require.NoError(t, err)
	require.Equal(t, "succeeded", lookup.Bindings[0].Operation.State)
	require.Equal(t, result.RemoteID, lookup.Bindings[0].Operation.RemoteID)
	require.NoError(t, repo.MarkUncertain(ctx, permit, file.PublicID, "commit_ack_lost"))
	require.ErrorIs(t, repo.AuthorizeGeneration(ctx, permit), service.ErrFileLeaseConflict)
	// A new authorized request can reuse the recorded copy after the old
	// preparation lease ends; it never allocates a second upload operation.
	reuse, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "reuse-copy"))
	require.NoError(t, err)
	require.Equal(t, permit.Files[0].Operation.ID, reuse.Files[0].Operation.ID)
	require.NoError(t, repo.AuthorizeGeneration(ctx, reuse))
}

func managedEnsureResult(permit *service.ManagedFileReservation) managedfiles.EnsureResult {
	binding := permit.Files[0]
	return managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: permit.Selection.Identity,
		OperationID: binding.Operation.ID, FileRef: binding.File.PublicID, RemoteID: "file-bps-confirmed",
		Bytes: binding.File.Bytes, SHA256: binding.File.SHA256, Filename: binding.File.Filename, MIME: binding.File.MIME, InputTokens: 17}
}

func TestManagedFileLedgerRetirementFencesGenerationButRecordsRemoteFact(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	permit, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "retiring-upload"))
	require.NoError(t, err)
	require.NoError(t, repo.StartOperation(ctx, permit, file.PublicID))
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `SELECT id FROM gateway_files WHERE id=$1 FOR UPDATE`, file.ID)
	require.NoError(t, err)
	disposition, err := repo.FenceForRetirement(ctx, tx, service.FileLifecycleSnapshot{
		ID: file.ID, UserID: owner.UserID, GroupID: owner.GroupID, Version: file.Version,
		PublicID: file.PublicID, State: "ready", RemoteActivityPossible: true,
	})
	require.NoError(t, err)
	require.Equal(t, service.FileRemoteUncertain, disposition.Kind)
	_, err = tx.ExecContext(ctx, `UPDATE gateway_files SET state='retired',retired_at=clock_timestamp(),retire_reason='expired',read_drain_until=clock_timestamp()+INTERVAL '1 minute',lifecycle_version=lifecycle_version+1 WHERE id=$1`, file.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	active, err := repo.CompleteOperation(ctx, permit, file.PublicID, managedEnsureResult(permit))
	require.NoError(t, err)
	require.False(t, active)
	require.ErrorIs(t, repo.AuthorizeGeneration(ctx, permit), service.ErrFileNotFound)
	tx, err = integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	disposition, err = repo.InspectForLocalPurge(ctx, tx, service.FileLifecycleSnapshot{ID: file.ID, RemoteActivityPossible: true})
	require.NoError(t, err)
	require.Equal(t, service.FileRemoteConfirmed, disposition.Kind)
	require.NotEmpty(t, disposition.Revision)
}

func TestManagedFileLedgerStartRechecksExpiryAfterFileLock(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	permit, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "expiry-upload"))
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()+INTERVAL '100 milliseconds' WHERE id=$1`, file.ID)
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() { finished <- repo.StartOperation(ctx, permit, file.PublicID) }()
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-finished, service.ErrFileNotFound)
	var remotePossible bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT remote_activity_possible FROM gateway_files WHERE id=$1`, file.ID).Scan(&remotePossible))
	require.False(t, remotePossible)
}

func TestManagedFileLedgerMissingEvidenceNeverMeansNone(t *testing.T) {
	repo, _, file := managedLedgerFixture(t)
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	disposition, err := repo.InspectForLocalPurge(ctx, tx, service.FileLifecycleSnapshot{ID: file.ID})
	require.NoError(t, err)
	require.Equal(t, service.FileRemoteNone, disposition.Kind)
	_, err = tx.ExecContext(ctx, `UPDATE gateway_files SET remote_activity_possible=true WHERE id=$1`, file.ID)
	require.NoError(t, err)
	_, err = repo.InspectForLocalPurge(ctx, tx, service.FileLifecycleSnapshot{ID: file.ID, RemoteActivityPossible: true})
	require.ErrorIs(t, err, service.ErrFileRemoteGuardUnavailable)
}

func managedAdditionalFile(t *testing.T, owner service.FileOwner) *service.StoredFile {
	t.Helper()
	nonce := time.Now().UnixNano()
	f := &service.StoredFile{PublicID: fmt.Sprintf("file-managed-extra-%d", nonce), UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, StorageKey: fmt.Sprintf("%032x", nonce), Filename: "extra.txt", MIME: "text/plain", Purpose: "user_data", ReservedBytes: 100, UploadToken: fmt.Sprintf("extra-%d", nonce)}
	repo := NewFileRepository(integrationDB)
	require.NoError(t, repo.Reserve(context.Background(), f, 1000, 1<<60, time.Minute))
	require.NoError(t, repo.CommitUpload(context.Background(), f.ID, f.UploadToken, 3, strings.Repeat("c", 64), "user_data"))
	f, err := repo.GetForOwner(context.Background(), owner.UserID, owner.GroupID, f.PublicID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec(`DELETE FROM gateway_file_remote_operations WHERE file_id=$1`, f.ID)
		require.NoError(t, err)
		_, err = integrationDB.Exec(`DELETE FROM gateway_file_bindings WHERE file_id=$1`, f.ID)
		require.NoError(t, err)
	})
	return f
}

func TestManagedFileLedgerRejectedBatchDoesNotFreezeUnboundFile(t *testing.T) {
	repo, owner, first := managedLedgerFixture(t)
	second := managedAdditionalFile(t, owner)
	ctx := context.Background()
	started, err := repo.Reserve(ctx, managedReserve(owner, second, 71, "old-upload"))
	require.NoError(t, err)
	require.NoError(t, repo.StartOperation(ctx, started, second.PublicID))
	_, err = integrationDB.Exec(`UPDATE gateway_file_bindings SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE file_id=$1`, second.ID)
	require.NoError(t, err)
	in := managedReserve(owner, first, 71, "batch-attempt")
	in.Files = append(in.Files, service.ManagedFileVersion{PublicID: second.PublicID, Version: second.Version})
	_, err = repo.Reserve(ctx, in)
	require.ErrorIs(t, err, service.ErrManagedFileUncertain)
	lookup, err := repo.LookupBindings(ctx, owner, []string{first.PublicID})
	require.NoError(t, err)
	require.Zero(t, lookup.AccountID, "rejected batch must not permanently claim another file")
	_, err = repo.Reserve(ctx, managedReserve(owner, first, 72, "other-account"))
	require.NoError(t, err)
	lookup, err = repo.LookupBindings(ctx, owner, []string{second.PublicID})
	require.NoError(t, err)
	require.Equal(t, "uncertain", lookup.Bindings[0].Operation.State)
}

func TestManagedFileLedgerGenerationRechecksAllExpiryAfterBatchLock(t *testing.T) {
	repo, owner, first := managedLedgerFixture(t)
	second := managedAdditionalFile(t, owner)
	ctx := context.Background()
	in := managedReserve(owner, first, 71, "generation-batch")
	in.Files = append(in.Files, service.ManagedFileVersion{PublicID: second.PublicID, Version: second.Version})
	permit, err := repo.Reserve(ctx, in)
	require.NoError(t, err)
	for _, binding := range permit.Files {
		require.NoError(t, repo.StartOperation(ctx, permit, binding.File.PublicID))
		one := *permit
		one.Files = []service.ManagedFileBinding{binding}
		active, err := repo.CompleteOperation(ctx, permit, binding.File.PublicID, managedEnsureResult(&one))
		require.NoError(t, err)
		require.True(t, active)
	}
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`SELECT id FROM gateway_files WHERE id=$1 FOR UPDATE`, second.ID)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE gateway_files SET expires_at=clock_timestamp()+INTERVAL '150 milliseconds' WHERE id=$1`, first.ID)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- repo.AuthorizeGeneration(ctx, permit) }()
	time.Sleep(250 * time.Millisecond)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-done, service.ErrFileNotFound, "earlier file expired while waiting for a later file lock")
}

func TestManagedFileLedgerReleaseOnlyOwnLeaseAndRetainsCopy(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	old, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "first-request"))
	require.NoError(t, err)
	require.NoError(t, repo.StartOperation(ctx, old, file.PublicID))
	active, err := repo.CompleteOperation(ctx, old, file.PublicID, managedEnsureResult(old))
	require.NoError(t, err)
	require.True(t, active)
	require.NoError(t, repo.ReleaseReservation(ctx, old))
	require.ErrorIs(t, repo.AuthorizeGeneration(ctx, old), service.ErrFileLeaseConflict)
	fresh, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "second-request"))
	require.NoError(t, err)
	require.Equal(t, old.Files[0].Operation.ID, fresh.Files[0].Operation.ID)
	require.NoError(t, repo.ReleaseReservation(ctx, old), "stale cleanup cannot release a newer owner")
	require.NoError(t, repo.AuthorizeGeneration(ctx, fresh))
	require.NoError(t, repo.ReleaseReservation(ctx, fresh))
}

func TestManagedFileLedgerDatabaseRejectsUnverifiedConfirmedTokens(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	permit, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "token-check"))
	require.NoError(t, err)
	require.NoError(t, repo.StartOperation(ctx, permit, file.PublicID))
	_, err = integrationDB.Exec(`UPDATE gateway_file_remote_operations SET state='succeeded',remote_id='file-bps-forged',completed_at=clock_timestamp(),input_tokens=NULL WHERE operation_id=$1`, permit.Files[0].Operation.ID)
	require.Error(t, err, "confirmed copies require a present server token count, including valid zero")
	result := managedEnsureResult(permit)
	result.InputTokens = 0
	active, err := repo.CompleteOperation(ctx, permit, file.PublicID, result)
	require.NoError(t, err)
	require.True(t, active)
	for _, statement := range []string{
		`UPDATE gateway_file_remote_operations SET remote_id='file-bps-other' WHERE operation_id=$1`,
		`UPDATE gateway_file_remote_operations SET sha256='` + strings.Repeat("d", 64) + `' WHERE operation_id=$1`,
		`UPDATE gateway_file_remote_operations SET state='uncertain',remote_id=NULL,input_tokens=NULL WHERE operation_id=$1`,
	} {
		_, err = integrationDB.Exec(statement, permit.Files[0].Operation.ID)
		require.Error(t, err, "confirmed copy and original identity are immutable")
	}
}

func TestManagedFileLedgerStartRollbackNeverLeavesWriteAheadOnly(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	permit, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "rollback-upload"))
	require.NoError(t, err)
	// A real database write failure after setting the write-ahead flag must roll
	// back both changes and preserve the original retryable reserved operation.
	_, err = integrationDB.Exec(`CREATE FUNCTION managed_ledger_test_abort_started() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation_id='` + permit.Files[0].Operation.ID + `' AND NEW.state='started' THEN RAISE EXCEPTION 'owned failure injection'; END IF; RETURN NEW; END; $$; CREATE TRIGGER managed_ledger_test_abort_started BEFORE UPDATE ON gateway_file_remote_operations FOR EACH ROW EXECUTE FUNCTION managed_ledger_test_abort_started()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec(`DROP TRIGGER IF EXISTS managed_ledger_test_abort_started ON gateway_file_remote_operations; DROP FUNCTION IF EXISTS managed_ledger_test_abort_started()`)
		require.NoError(t, err)
	})
	require.Error(t, repo.StartOperation(ctx, permit, file.PublicID))
	var possible bool
	require.NoError(t, integrationDB.QueryRow(`SELECT remote_activity_possible FROM gateway_files WHERE id=$1`, file.ID).Scan(&possible))
	require.False(t, possible)
	lookup, err := repo.LookupBindings(ctx, owner, []string{file.PublicID})
	require.NoError(t, err)
	require.Equal(t, "reserved", lookup.Bindings[0].Operation.State)
	_, err = integrationDB.Exec(`DROP TRIGGER managed_ledger_test_abort_started ON gateway_file_remote_operations; DROP FUNCTION managed_ledger_test_abort_started()`)
	require.NoError(t, err)
	require.NoError(t, repo.StartOperation(ctx, permit, file.PublicID))
}

func TestManagedFileLedgerRejectsForeignScopeVersionAndResultMutation(t *testing.T) {
	repo, owner, file := managedLedgerFixture(t)
	ctx := context.Background()
	for _, scope := range []service.FileOwner{
		{UserID: owner.UserID + 1, GroupID: owner.GroupID, APIKeyID: owner.APIKeyID},
		{UserID: owner.UserID, GroupID: owner.GroupID + 1, APIKeyID: owner.APIKeyID},
	} {
		_, err := repo.LookupBindings(ctx, scope, []string{file.PublicID})
		require.ErrorIs(t, err, service.ErrFileNotFound)
	}
	wrong := managedReserve(owner, file, 71, "wrong-version")
	wrong.Files[0].Version++
	_, err := repo.Reserve(ctx, wrong)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
	permit, err := repo.Reserve(ctx, managedReserve(owner, file, 71, "result-verification"))
	require.NoError(t, err)
	changed := managedReserve(owner, file, 71, "changed-identity")
	changed.Selection.Identity.CredentialGeneration = "epoch-2"
	_, err = repo.Reserve(ctx, changed)
	require.ErrorIs(t, err, service.ErrManagedFileBindingConflict)
	require.NoError(t, repo.StartOperation(ctx, permit, file.PublicID))
	result := managedEnsureResult(permit)
	result.SHA256 = strings.Repeat("e", 64)
	_, err = repo.CompleteOperation(ctx, permit, file.PublicID, result)
	require.ErrorIs(t, err, service.ErrInvalidFile)
	lookup, err := repo.LookupBindings(ctx, owner, []string{file.PublicID})
	require.NoError(t, err)
	require.Equal(t, "started", lookup.Bindings[0].Operation.State)
	require.Empty(t, lookup.Bindings[0].Operation.RemoteID)
}
