//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func managedRecoveryFixture(t *testing.T) (*ManagedFileLedgerRepository, *service.ManagedFileReservation) {
	t.Helper()
	return managedRecoveryPrepared(t, false)
}

func managedRecoveryPrepared(t *testing.T, mixed bool) (*ManagedFileLedgerRepository, *service.ManagedFileReservation) {
	t.Helper()
	f := newLifecycleHTTPFixture(t)
	owner := service.FileOwner{UserID: f.Owner.UserID, APIKeyID: f.Owner.ID, GroupID: f.DBGroupID}
	ledger := NewManagedFileRepository(integrationDB)
	files := NewFileRepositoryWithRemoteLifecycle(integrationDB, ledger)
	in := service.ManagedFileReserveInput{Owner: owner, LeaseToken: "recovery-fixture-original", Lease: time.Minute}
	for _, name := range []string{"recovery-first.txt", "recovery-second.txt"} {
		ref := f.upload(t, f.Owner, name, "user_data", nil)
		file, err := files.GetForOwner(context.Background(), owner.UserID, owner.GroupID, ref)
		require.NoError(t, err)
		if in.Selection.AccountID == 0 {
			in.Selection = managedReserve(owner, file, 71, "unused").Selection
		}
		in.Files = append(in.Files, service.ManagedFileVersion{PublicID: ref, Version: file.Version})
	}
	permit, err := ledger.Reserve(context.Background(), in)
	require.NoError(t, err)
	for i, binding := range permit.Files {
		if mixed && i == 1 {
			continue
		}
		require.NoError(t, ledger.StartOperation(context.Background(), permit, binding.File.PublicID))
		one := *permit
		one.Files = []service.ManagedFileBinding{binding}
		active, err := ledger.CompleteOperation(context.Background(), permit, binding.File.PublicID, managedEnsureResult(&one))
		require.NoError(t, err)
		require.True(t, active)
	}
	require.NoError(t, ledger.ReleaseReservation(context.Background(), permit))
	in.LeaseToken = "recovery-fixture-current"
	permit, err = ledger.Reserve(context.Background(), in)
	require.NoError(t, err)
	if !mixed {
		require.NoError(t, ledger.AuthorizeGeneration(context.Background(), permit))
	}
	return ledger, permit
}

func TestManagedFileRecoveryPreservesUnsentNonmissingUpload(t *testing.T) {
	ledger, original := managedRecoveryPrepared(t, true)
	ctx := context.Background()
	missing, unsent := original.Files[0], original.Files[1]
	require.Equal(t, "succeeded", missing.Operation.State)
	require.Equal(t, "reserved", unsent.Operation.State)
	recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{missing.File.PublicID}})
	require.NoError(t, err)
	require.Equal(t, unsent, recovered.Files[1])
	for _, binding := range recovered.Files {
		require.NoError(t, ledger.StartOperation(ctx, recovered, binding.File.PublicID))
		one := *recovered
		one.Files = []service.ManagedFileBinding{binding}
		active, err := ledger.CompleteOperation(ctx, recovered, binding.File.PublicID, managedEnsureResult(&one))
		require.NoError(t, err)
		require.True(t, active)
	}
	require.NoError(t, ledger.AuthorizeGeneration(ctx, recovered))
	var operations int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM gateway_file_remote_operations WHERE file_id=$1`, unsent.File.ID).Scan(&operations))
	require.Equal(t, 1, operations, "unrelated first upload retains its original operation rather than being recovered")
}

func managedRecoveryOperationFacts(t *testing.T, operation string) string {
	t.Helper()
	var facts string
	require.NoError(t, integrationDB.QueryRow(`SELECT to_jsonb(o)::text FROM gateway_file_remote_operations o WHERE operation_id=$1`, operation).Scan(&facts))
	return facts
}

func managedRecoveryComplete(t *testing.T, ledger *ManagedFileLedgerRepository, permit *service.ManagedFileReservation, ref string) {
	t.Helper()
	var binding service.ManagedFileBinding
	for _, candidate := range permit.Files {
		if candidate.File.PublicID == ref {
			binding = candidate
		}
	}
	require.NotNil(t, binding.File)
	require.NoError(t, ledger.StartOperation(context.Background(), permit, ref))
	one := *permit
	one.Files = []service.ManagedFileBinding{binding}
	result := managedEnsureResult(&one)
	result.RemoteID = "file-bps-" + binding.Operation.ID
	active, err := ledger.CompleteOperation(context.Background(), permit, ref, result)
	require.NoError(t, err)
	require.True(t, active)
}

func managedRecoveryReacquire(t *testing.T, ledger *ManagedFileLedgerRepository, permit *service.ManagedFileReservation) *service.ManagedFileReservation {
	t.Helper()
	require.NoError(t, ledger.ReleaseReservation(context.Background(), permit))
	in := service.ManagedFileReserveInput{Owner: permit.Owner, Selection: permit.Selection, LeaseToken: fmt.Sprintf("recovery-round-%d", time.Now().UnixNano()), Lease: time.Minute}
	for _, binding := range permit.Files {
		in.Files = append(in.Files, service.ManagedFileVersion{PublicID: binding.File.PublicID, Version: binding.File.Version})
	}
	next, err := ledger.Reserve(context.Background(), in)
	require.NoError(t, err)
	return next
}

func managedRecoveryOperationsExcept(t *testing.T, fileID int64, except string) string {
	t.Helper()
	var facts string
	require.NoError(t, integrationDB.QueryRow(`SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id)::text FROM gateway_file_remote_operations o WHERE file_id=$1 AND operation_id<>$2`, fileID, except).Scan(&facts))
	return facts
}

func TestManagedFileRecoveryOperationRemainsCurrentAfterClockRollback(t *testing.T) {
	for _, equalPrior := range []bool{false, true} {
		t.Run(fmt.Sprintf("equal_prior_%t", equalPrior), func(t *testing.T) {
			ledger, original := managedRecoveryFixture(t)
			ref := original.Files[0].File.PublicID
			fileID := original.Files[0].File.ID
			if equalPrior {
				first, err := ledger.ReserveRecovery(context.Background(), service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{ref}})
				require.NoError(t, err)
				managedRecoveryComplete(t, ledger, first, ref)
				require.NoError(t, ledger.ReleaseReservation(context.Background(), first))
				original = first
			}
			// Simulate old evidence sharing a future microsecond after a database
			// clock correction. No randomized operation-ID ordering is assumed.
			_, err := integrationDB.Exec(`UPDATE gateway_file_remote_operations SET created_at='2100-01-01T00:00:00Z'::timestamptz WHERE file_id=$1`, fileID)
			require.NoError(t, err)
			if equalPrior {
				original = managedRecoveryReacquire(t, ledger, original)
			}
			oldFacts := managedRecoveryOperationsExcept(t, fileID, "")
			var previous time.Time
			require.NoError(t, integrationDB.QueryRow(`SELECT MAX(created_at) FROM gateway_file_remote_operations WHERE file_id=$1`, fileID).Scan(&previous))
			recovered, err := ledger.ReserveRecovery(context.Background(), service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{ref}})
			require.NoError(t, err)
			var created time.Time
			require.NoError(t, integrationDB.QueryRow(`SELECT created_at FROM gateway_file_remote_operations WHERE operation_id=$1`, recovered.Files[0].Operation.ID).Scan(&created))
			require.True(t, created.After(previous), "current-operation selection must survive future and equal old timestamps")
			managedRecoveryComplete(t, ledger, recovered, ref)
			require.NoError(t, ledger.AuthorizeGeneration(context.Background(), recovered))
			lookup, err := ledger.LookupBindings(context.Background(), recovered.Owner, []string{ref})
			require.NoError(t, err)
			require.Equal(t, recovered.Files[0].Operation.ID, lookup.Bindings[0].Operation.ID)
			require.Equal(t, oldFacts, managedRecoveryOperationsExcept(t, fileID, recovered.Files[0].Operation.ID))
		})
	}
}

func TestManagedFileRecoverySerializesConcurrentLeaders(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	missing := original.Files[0]
	oldFacts := managedRecoveryOperationFacts(t, missing.Operation.ID)
	gate, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT id FROM gateway_files WHERE id=$1 FOR UPDATE`, missing.File.ID)
	require.NoError(t, err)
	type outcome struct {
		permit *service.ManagedFileReservation
		err    error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			permit, err := NewManagedFileRepository(integrationDB).ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{missing.File.PublicID}})
			results <- outcome{permit, err}
		}()
	}
	require.Eventually(t, func() bool {
		var waiting int
		err := integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%WHERE id=$1 AND user_id=$2 AND group_id=$3 FOR UPDATE%'`).Scan(&waiting)
		return err == nil && waiting >= 2
	}, 3*time.Second, 5*time.Millisecond, "both real recovery transactions must contend on the locked file")
	require.NoError(t, gate.Commit())
	var winner *service.ManagedFileReservation
	leaders, stale := 0, 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			leaders++
			winner = result.permit
		} else if errors.Is(result.err, service.ErrFileLeaseConflict) {
			stale++
			require.Nil(t, result.permit)
		} else {
			require.NoError(t, result.err)
		}
	}
	require.Equal(t, 1, leaders)
	require.Equal(t, 1, stale)
	var attempts int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_file_remote_operations WHERE file_id=$1 AND kind='recovery'`, missing.File.ID).Scan(&attempts))
	require.Equal(t, 1, attempts)
	require.Equal(t, oldFacts, managedRecoveryOperationFacts(t, missing.Operation.ID))
	managedRecoveryComplete(t, ledger, winner, missing.File.PublicID)
	require.NoError(t, ledger.AuthorizeGeneration(ctx, winner))
}

func TestManagedFileRecoveryNeverSupersedesStartedOrUncertain(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx := context.Background()
	missing := original.Files[0]
	oldFacts := managedRecoveryOperationFacts(t, missing.Operation.ID)
	recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{missing.File.PublicID}})
	require.NoError(t, err)
	require.NoError(t, ledger.StartOperation(ctx, recovered, missing.File.PublicID))
	require.ErrorIs(t, ledger.StartOperation(ctx, recovered, missing.File.PublicID), service.ErrManagedFileUncertain)
	for _, state := range []string{"started", "uncertain"} {
		if state == "uncertain" {
			require.NoError(t, ledger.MarkUncertain(ctx, recovered, missing.File.PublicID, "recovery_ack_lost"))
		}
		before := managedLifecycleRemoteFacts(t, missing.File.ID)
		second, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: recovered, FileRefs: []string{missing.File.PublicID}})
		require.ErrorIs(t, err, service.ErrManagedFileUncertain, state)
		require.Nil(t, second)
		require.Equal(t, before, managedLifecycleRemoteFacts(t, missing.File.ID), "unknown outcomes cannot allocate or rewrite an operation")
	}
	one := *recovered
	one.Files = []service.ManagedFileBinding{recovered.Files[0]}
	activated, err := ledger.CompleteOperation(ctx, recovered, missing.File.PublicID, managedEnsureResult(&one))
	require.NoError(t, err)
	require.True(t, activated, "a confirmed late acknowledgement can still complete the same live operation")
	require.NoError(t, ledger.AuthorizeGeneration(ctx, recovered))
	require.Equal(t, oldFacts, managedRecoveryOperationFacts(t, missing.Operation.ID))
}

func managedRecoveryBatchFacts(t *testing.T, permit *service.ManagedFileReservation) string {
	t.Helper()
	var facts string
	for _, binding := range permit.Files {
		facts += managedLifecycleRemoteFacts(t, binding.File.ID)
	}
	return facts
}

func managedRecoveryAllRefs(permit *service.ManagedFileReservation) []string {
	refs := make([]string, 0, len(permit.Files))
	for _, binding := range permit.Files {
		refs = append(refs, binding.File.PublicID)
	}
	sort.Strings(refs)
	return refs
}

func TestManagedFileRecoveryBudgetPersistsAcrossRequestsAndUsesDatabaseWindow(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx := context.Background()
	target := original.Files[1]
	oldFacts := managedRecoveryOperationFacts(t, target.Operation.ID)
	for attempt := 0; attempt < 3; attempt++ {
		recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{target.File.PublicID}})
		require.NoError(t, err)
		managedRecoveryComplete(t, ledger, recovered, target.File.PublicID)
		original = managedRecoveryReacquire(t, ledger, recovered)
	}
	require.Equal(t, oldFacts, managedRecoveryOperationFacts(t, target.Operation.ID))
	before := managedRecoveryBatchFacts(t, original)
	// A fresh repository and request cannot reset persisted budget. Including
	// an otherwise valid first target cannot partially mutate the batch.
	next, err := NewManagedFileRepository(integrationDB).ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: managedRecoveryAllRefs(original)})
	require.ErrorIs(t, err, service.ErrManagedFileRecoveryBudget)
	require.Nil(t, next)
	require.Equal(t, before, managedRecoveryBatchFacts(t, original))
	require.NoError(t, ledger.AuthorizeGeneration(ctx, original))
	// Age every historical row together, preserving current-operation ordering.
	_, err = integrationDB.Exec(`UPDATE gateway_file_remote_operations SET created_at=created_at-interval '2 hours' WHERE file_id=$1`, target.File.ID)
	require.NoError(t, err)
	next, err = ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{target.File.PublicID}})
	require.NoError(t, err)
	managedRecoveryComplete(t, ledger, next, target.File.PublicID)
	original = managedRecoveryReacquire(t, ledger, next)
	// Future-dated facts are conservatively counted after clock rollback.
	_, err = integrationDB.Exec(`UPDATE gateway_file_remote_operations SET created_at=created_at+interval '1 day' WHERE file_id=$1`, target.File.ID)
	require.NoError(t, err)
	before = managedRecoveryBatchFacts(t, original)
	next, err = ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{target.File.PublicID}})
	require.ErrorIs(t, err, service.ErrManagedFileRecoveryBudget)
	require.Nil(t, next)
	require.Equal(t, before, managedRecoveryBatchFacts(t, original))
	var attempts int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM gateway_file_remote_operations WHERE file_id=$1 AND kind='recovery'`, target.File.ID).Scan(&attempts))
	require.Equal(t, 4, attempts, "out-of-window success permits one new operation without deleting historical attempts")
}

func TestManagedFileRecoveryRollsBackWholeBatchOnDatabaseFailure(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx := context.Background()
	first, second := original.Files[0].File, original.Files[1].File
	trigger := fmt.Sprintf("managed_recovery_abort_%d", second.ID)
	_, err := integrationDB.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM gateway_file_remote_operations WHERE file_id=%d AND kind='recovery') THEN RAISE EXCEPTION 'first target was not inserted'; END IF;
 RAISE EXCEPTION 'owned second target recovery failure'; END; $$;
 CREATE TRIGGER %s BEFORE INSERT ON gateway_file_remote_operations FOR EACH ROW WHEN (NEW.file_id=%d AND NEW.kind='recovery') EXECUTE FUNCTION %s()`, trigger, first.ID, trigger, second.ID, trigger))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON gateway_file_remote_operations; DROP FUNCTION IF EXISTS %s()`, trigger, trigger))
		require.NoError(t, err)
	})
	before := managedRecoveryBatchFacts(t, original)
	recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: managedRecoveryAllRefs(original)})
	require.ErrorContains(t, err, "owned second target recovery failure")
	require.Nil(t, recovered)
	require.Equal(t, before, managedRecoveryBatchFacts(t, original), "the first insert and binding update must roll back with the second target")
	require.NoError(t, ledger.AuthorizeGeneration(ctx, original))
	_, err = integrationDB.Exec(fmt.Sprintf(`DROP TRIGGER %s ON gateway_file_remote_operations; DROP FUNCTION %s()`, trigger, trigger))
	require.NoError(t, err)
	recovered, err = ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: managedRecoveryAllRefs(original)})
	require.NoError(t, err)
	for _, binding := range recovered.Files {
		managedRecoveryComplete(t, ledger, recovered, binding.File.PublicID)
	}
	require.NoError(t, ledger.AuthorizeGeneration(ctx, recovered))
}

func managedRecoveryClone(permit *service.ManagedFileReservation) *service.ManagedFileReservation {
	clone := *permit
	clone.Files = append([]service.ManagedFileBinding(nil), permit.Files...)
	for i := range clone.Files {
		file := *clone.Files[i].File
		clone.Files[i].File = &file
	}
	return &clone
}

func TestManagedFileRecoveryRejectsForeignIdentityAndStaleBatchWithoutMutation(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		change func(*service.ManagedFileReservation)
		want   error
	}{
		{"owner", func(p *service.ManagedFileReservation) { p.Owner.UserID++ }, service.ErrFileNotFound},
		{"group", func(p *service.ManagedFileReservation) { p.Owner.GroupID++ }, service.ErrFileNotFound},
		{"account", func(p *service.ManagedFileReservation) { p.Selection.AccountID++ }, service.ErrManagedFileBindingConflict},
		{"backend", func(p *service.ManagedFileReservation) { p.Selection.Identity.BackendID += "-other" }, service.ErrManagedFileBindingConflict},
		{"subject", func(p *service.ManagedFileReservation) {
			p.Selection.Identity.SubjectFingerprint = "sha256:" + strings.Repeat("c", 64)
		}, service.ErrManagedFileBindingConflict},
		{"credential_generation", func(p *service.ManagedFileReservation) { p.Selection.Identity.CredentialGeneration = "epoch-other" }, service.ErrManagedFileBindingConflict},
		{"second_binding_version", func(p *service.ManagedFileReservation) { p.Files[1].Version++ }, service.ErrFileLeaseConflict},
		{"second_file_version", func(p *service.ManagedFileReservation) { p.Files[1].File.Version++ }, service.ErrFileLeaseConflict},
		{"second_file_payload", func(p *service.ManagedFileReservation) { p.Files[1].File.SHA256 = strings.Repeat("f", 64) }, service.ErrInvalidFile},
		{"lease_token", func(p *service.ManagedFileReservation) { p.LeaseToken += "-other" }, service.ErrFileLeaseConflict},
		{"second_lease_deadline", func(p *service.ManagedFileReservation) {
			p.Files[1].LeaseUntil = p.Files[1].LeaseUntil.Add(time.Second)
		}, service.ErrFileLeaseConflict},
		{"second_operation", func(p *service.ManagedFileReservation) { p.Files[1].Operation.ID = strings.Repeat("f", 32) }, service.ErrFileLeaseConflict},
		{"second_remote", func(p *service.ManagedFileReservation) { p.Files[1].Operation.RemoteID = "file-not-the-original" }, service.ErrManagedFileBindingConflict},
		{"second_tokens", func(p *service.ManagedFileReservation) { p.Files[1].Operation.InputTokens++ }, service.ErrManagedFileBindingConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := managedRecoveryClone(original)
			tc.change(changed)
			for i := range changed.Files {
				changed.Files[i].Selection = changed.Selection
				changed.Files[i].LeaseToken = changed.LeaseToken
			}
			before := managedRecoveryBatchFacts(t, original)
			recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: changed, FileRefs: managedRecoveryAllRefs(changed)})
			require.ErrorIs(t, err, tc.want)
			require.Nil(t, recovered)
			require.Equal(t, before, managedRecoveryBatchFacts(t, original))
		})
	}
	require.NoError(t, ledger.AuthorizeGeneration(ctx, original))
	// Even a snapshot matching the actual expired lease cannot make it live.
	require.NoError(t, integrationDB.QueryRow(`UPDATE gateway_file_bindings SET lease_until=clock_timestamp()-interval '1 second' WHERE file_id=$1 RETURNING lease_until`, original.Files[1].File.ID).Scan(&original.Files[1].LeaseUntil))
	before := managedRecoveryBatchFacts(t, original)
	recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: managedRecoveryAllRefs(original)})
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
	require.Nil(t, recovered)
	require.Equal(t, before, managedRecoveryBatchFacts(t, original))
}

func TestManagedFileRecoveryRetirementPreservesLateConfirmationWithoutActivation(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx := context.Background()
	missing := original.Files[0]
	oldFacts := managedRecoveryOperationFacts(t, missing.Operation.ID)
	recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{missing.File.PublicID}})
	require.NoError(t, err)
	require.NoError(t, ledger.StartOperation(ctx, recovered, missing.File.PublicID))
	_, err = integrationDB.Exec(`UPDATE gateway_files SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, missing.File.ID)
	require.NoError(t, err)
	files := NewFileRepositoryWithRemoteLifecycle(integrationDB, ledger)
	retired, err := files.RetireExpired(ctx, missing.File.ID, time.Minute)
	require.NoError(t, err)
	require.True(t, retired)
	var operationState string
	require.NoError(t, integrationDB.QueryRow(`SELECT state FROM gateway_file_remote_operations WHERE operation_id=$1`, recovered.Files[0].Operation.ID).Scan(&operationState))
	require.Equal(t, "uncertain", operationState)
	one := *recovered
	one.Files = []service.ManagedFileBinding{recovered.Files[0]}
	result := managedEnsureResult(&one)
	result.RemoteID = "file-bps-late-recovery"
	active, err := ledger.CompleteOperation(ctx, recovered, missing.File.PublicID, result)
	require.NoError(t, err)
	require.False(t, active)
	require.ErrorIs(t, ledger.AuthorizeGeneration(ctx, recovered), service.ErrFileNotFound)
	_, err = ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: recovered, FileRefs: []string{missing.File.PublicID}})
	require.ErrorIs(t, err, service.ErrFileNotFound)
	var fileState, remote string
	var operation, lease sql.NullString
	var tokens, version int64
	require.NoError(t, integrationDB.QueryRow(`SELECT f.state,f.lifecycle_version,b.active_operation_id,b.lease_token,o.state,o.remote_id,o.input_tokens
 FROM gateway_files f JOIN gateway_file_bindings b ON b.file_id=f.id JOIN gateway_file_remote_operations o ON o.file_id=f.id WHERE o.operation_id=$1`, recovered.Files[0].Operation.ID).Scan(&fileState, &version, &operation, &lease, &operationState, &remote, &tokens))
	require.Equal(t, "retired", fileState)
	require.Equal(t, missing.File.Version+1, version)
	require.False(t, operation.Valid)
	require.False(t, lease.Valid)
	require.Equal(t, "succeeded", operationState)
	require.Equal(t, "file-bps-late-recovery", remote)
	require.EqualValues(t, 17, tokens)
	require.Equal(t, oldFacts, managedRecoveryOperationFacts(t, missing.Operation.ID))
	retiredFacts := managedLifecycleRemoteFacts(t, missing.File.ID)
	confirmedFacts := make(map[string]string, len(recovered.Files))
	for _, binding := range recovered.Files {
		confirmedFacts[binding.Operation.ID] = managedRecoveryOperationFacts(t, binding.Operation.ID)
	}
	require.NoError(t, ledger.ReleaseReservation(ctx, recovered))
	require.Equal(t, retiredFacts, managedLifecycleRemoteFacts(t, missing.File.ID), "stale plan cleanup cannot change the retired binding or its confirmed facts")
	for operationID, facts := range confirmedFacts {
		require.Equal(t, facts, managedRecoveryOperationFacts(t, operationID), "releasing other live members must retain every confirmed operation")
	}
}

func TestManagedFileRecoveryRechecksEveryDeadlineAfterBatchLocks(t *testing.T) {
	for _, kind := range []string{"file", "lease"} {
		t.Run(kind, func(t *testing.T) {
			ledger, original := managedRecoveryFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			first, second := original.Files[0].File, original.Files[1].File
			gate, err := integrationDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer gate.Rollback()
			_, err = gate.ExecContext(ctx, `SELECT id FROM gateway_files WHERE id=$1 FOR UPDATE`, second.ID)
			require.NoError(t, err)
			var deadline time.Time
			if kind == "file" {
				err = integrationDB.QueryRowContext(ctx, `UPDATE gateway_files SET expires_at=clock_timestamp()+interval '200 milliseconds' WHERE id=$1 RETURNING expires_at`, first.ID).Scan(&deadline)
			} else {
				err = integrationDB.QueryRowContext(ctx, `UPDATE gateway_file_bindings SET lease_until=clock_timestamp()+interval '200 milliseconds' WHERE file_id=$1 RETURNING lease_until`, first.ID).Scan(&deadline)
				original.Files[0].LeaseUntil = deadline
			}
			require.NoError(t, err)
			before := managedRecoveryBatchFacts(t, original)
			finished := make(chan error, 1)
			go func() {
				_, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: managedRecoveryAllRefs(original)})
				finished <- err
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%WHERE id=$1 AND user_id=$2 AND group_id=$3 FOR UPDATE%')`).Scan(&waiting)
				return err == nil && waiting
			}, time.Second, 5*time.Millisecond, "recovery must be waiting on the later batch member")
			require.Eventually(t, func() bool {
				var expired bool
				err := integrationDB.QueryRowContext(ctx, `SELECT clock_timestamp()>$1`, deadline).Scan(&expired)
				return err == nil && expired
			}, time.Second, 5*time.Millisecond)
			require.NoError(t, gate.Commit())
			if kind == "file" {
				require.ErrorIs(t, <-finished, service.ErrFileNotFound)
			} else {
				require.ErrorIs(t, <-finished, service.ErrFileLeaseConflict)
			}
			require.Equal(t, before, managedRecoveryBatchFacts(t, original))
		})
	}
}

func TestManagedFileRecoveryReplacesOnlyMissingCopyWithoutExtendingLease(t *testing.T) {
	ledger, original := managedRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	missing, unaffected := original.Files[0], original.Files[1]
	oldFacts := managedRecoveryOperationFacts(t, missing.Operation.ID)
	recovered, err := ledger.ReserveRecovery(ctx, service.ManagedFileRecoveryInput{Reservation: original, FileRefs: []string{missing.File.PublicID}})
	require.NoError(t, err)
	require.NotNil(t, recovered)
	require.Equal(t, original.Owner, recovered.Owner)
	require.Equal(t, original.Selection, recovered.Selection)
	require.Equal(t, original.LeaseToken, recovered.LeaseToken)
	require.Len(t, recovered.Files, 2)
	changed := recovered.Files[0]
	require.Equal(t, missing.File, changed.File)
	require.Equal(t, missing.Selection, changed.Selection)
	require.Equal(t, missing.Version+1, changed.Version)
	require.Equal(t, missing.LeaseToken, changed.LeaseToken)
	require.True(t, missing.LeaseUntil.Equal(changed.LeaseUntil), "recovery cannot extend the existing preparation lease")
	require.NotEqual(t, missing.Operation.ID, changed.Operation.ID)
	require.Equal(t, "reserved", changed.Operation.State)
	require.Empty(t, changed.Operation.RemoteID)
	require.Equal(t, unaffected, recovered.Files[1], "nonmissing bindings and permits stay unchanged")
	require.Equal(t, oldFacts, managedRecoveryOperationFacts(t, missing.Operation.ID), "the original confirmed copy remains immutable evidence")
	var kind string
	var active bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT o.kind,b.active_operation_id IS NOT NULL FROM gateway_file_remote_operations o JOIN gateway_file_bindings b ON b.file_id=o.file_id WHERE o.operation_id=$1`, changed.Operation.ID).Scan(&kind, &active))
	require.Equal(t, "recovery", kind)
	require.False(t, active)
	require.ErrorIs(t, ledger.AuthorizeGeneration(ctx, original), service.ErrFileLeaseConflict)
	require.ErrorIs(t, ledger.AuthorizeGeneration(ctx, recovered), service.ErrManagedFileNotReady)
	require.NoError(t, ledger.StartOperation(ctx, recovered, missing.File.PublicID))
	one := *recovered
	one.Files = []service.ManagedFileBinding{changed}
	result := managedEnsureResult(&one)
	result.RemoteID = "file-bps-recovered"
	activated, err := ledger.CompleteOperation(ctx, recovered, missing.File.PublicID, result)
	require.NoError(t, err)
	require.True(t, activated)
	require.NoError(t, ledger.AuthorizeGeneration(ctx, recovered))
	require.Equal(t, oldFacts, managedRecoveryOperationFacts(t, missing.Operation.ID))
}
