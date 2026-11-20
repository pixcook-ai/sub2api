//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func inlineReserveInput(t *testing.T) service.ManagedInlineReserveInput {
	t.Helper()
	return service.ManagedInlineReserveInput{
		Owner:     service.FileOwner{UserID: 971, APIKeyID: 972, GroupID: 973},
		Selection: service.ManagedFileSelection{AccountID: 974, Identity: managedfiles.Identity{BackendID: "inline-test", SubjectFingerprint: "sha256:" + strings.Repeat("b", 64), CredentialGeneration: "epoch-1"}},
		RequestID: fmt.Sprintf("inline-%s-%d", t.Name(), time.Now().UnixNano()), LeaseToken: "lease-inline", Lease: time.Minute,
		Files: []service.ManagedInlineDescriptor{{FileRef: "file-inline-first", Filename: "hello.txt", MIME: "text/plain", SHA256: strings.Repeat("a", 64), Bytes: 3}},
	}
}

func inlineResult(p *service.ManagedInlineReservation, index int) managedfiles.EnsureResult {
	f := p.Files[index]
	return managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: p.Selection.Identity, OperationID: f.Operation.ID, FileRef: f.Descriptor.FileRef, RemoteID: "file-remote-inline", Bytes: f.Descriptor.Bytes, SHA256: f.Descriptor.SHA256, Filename: f.Descriptor.Filename, MIME: f.Descriptor.MIME, InputTokens: 7}
}

func inlineCleanup(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM gateway_inline_remote_operations WHERE request_id=$1", id)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM gateway_inline_requests WHERE request_id=$1", id)
		require.NoError(t, err)
	})
}

func TestManagedInlineLedgerRequiresConfirmedStartedOperations(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	require.Len(t, p.Files, 1)
	require.NotEmpty(t, p.Files[0].Operation.ID)
	require.ErrorIs(t, repo.AuthorizeInlineGeneration(ctx, p), service.ErrManagedFileNotReady)
	_, err = repo.CompleteInline(ctx, p, in.Files[0].FileRef, inlineResult(p, 0))
	require.ErrorIs(t, err, service.ErrManagedFileNotReady)
	require.NoError(t, repo.StartInline(ctx, p, in.Files[0].FileRef))
	require.ErrorIs(t, repo.StartInline(ctx, p, in.Files[0].FileRef), service.ErrManagedFileUncertain)
	active, err := repo.CompleteInline(ctx, p, in.Files[0].FileRef, inlineResult(p, 0))
	require.NoError(t, err)
	require.True(t, active)
	require.NoError(t, repo.AuthorizeInlineGeneration(ctx, p))
	_, err = repo.ReserveInline(ctx, in)
	require.ErrorIs(t, err, service.ErrManagedFileBusy)
}

func TestManagedInlineLedgerKeepsLateConfirmationAfterRelease(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	ref := p.Files[0].Descriptor.FileRef
	require.NoError(t, repo.StartInline(ctx, p, ref))
	require.NoError(t, repo.ReleaseInline(ctx, p))
	active, err := repo.CompleteInline(ctx, p, ref, inlineResult(p, 0))
	require.NoError(t, err)
	require.False(t, active)
	require.ErrorIs(t, repo.AuthorizeInlineGeneration(ctx, p), service.ErrFileLeaseConflict)
	// A new repository instance sees exactly the same durable confirmation.
	require.NoError(t, NewManagedFileRepository(integrationDB).MarkInlineUncertain(ctx, p, ref, "ack_lost"))
	changed := inlineResult(p, 0)
	changed.RemoteID = "file-wrong-confirmation"
	_, err = NewManagedFileRepository(integrationDB).CompleteInline(ctx, p, ref, changed)
	require.ErrorIs(t, err, service.ErrManagedFileBindingConflict)
	active, err = repo.CompleteInline(ctx, p, ref, inlineResult(p, 0))
	require.NoError(t, err)
	require.False(t, active)
	require.NoError(t, repo.ReleaseInline(ctx, p))
	require.ErrorIs(t, repo.StartInline(ctx, p, ref), service.ErrFileLeaseConflict)
}

func TestManagedInlineLedgerRejectsDifferentOwnerAndRequestReuse(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	other := *p
	other.Owner.UserID++
	require.ErrorIs(t, repo.StartInline(ctx, &other, p.Files[0].Descriptor.FileRef), service.ErrManagedFileBindingConflict)
	clone := in
	clone.RequestID = in.RequestID + "-other"
	clone.Files = append([]service.ManagedInlineDescriptor(nil), in.Files...)
	clone.Files[0].FileRef = "file-inline-other"
	otherReservation, err := repo.ReserveInline(ctx, clone)
	require.NoError(t, err)
	require.NotEqual(t, p.Files[0].Operation.ID, otherReservation.Files[0].Operation.ID)
	inlineCleanup(t, clone.RequestID)
}

func TestManagedInlineLedgerConcurrentReservationAndStartHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	in := inlineReserveInput(t)
	inlineCleanup(t, in.RequestID)
	type answer struct {
		p   *service.ManagedInlineReservation
		err error
	}
	results := make(chan answer, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := NewManagedFileRepository(integrationDB).ReserveInline(ctx, in)
			results <- answer{p, err}
		}()
	}
	wg.Wait()
	close(results)
	var winner *service.ManagedInlineReservation
	win := 0
	for result := range results {
		if result.err == nil {
			win++
			winner = result.p
		} else {
			require.ErrorIs(t, result.err, service.ErrManagedFileBusy)
		}
	}
	require.Equal(t, 1, win)
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errors <- NewManagedFileRepository(integrationDB).StartInline(ctx, winner, in.Files[0].FileRef)
		}()
	}
	wg.Wait()
	close(errors)
	win = 0
	for err := range errors {
		if err == nil {
			win++
		} else {
			require.ErrorIs(t, err, service.ErrManagedFileUncertain)
		}
	}
	require.Equal(t, 1, win)
}

func cloneInlinePermit(p *service.ManagedInlineReservation) *service.ManagedInlineReservation {
	c := *p
	c.Files = append([]service.ManagedInlineBinding(nil), p.Files...)
	return &c
}

func TestManagedInlineLedgerValidatesEveryImmutablePermitFact(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	in.Files = append(in.Files, service.ManagedInlineDescriptor{FileRef: "file-inline-second", Filename: "second.pdf", MIME: "application/pdf", SHA256: strings.Repeat("c", 64), Bytes: 10})
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	changes := []struct {
		name   string
		change func(*service.ManagedInlineReservation)
	}{
		{"user", func(c *service.ManagedInlineReservation) { c.Owner.UserID++ }},
		{"key", func(c *service.ManagedInlineReservation) { c.Owner.APIKeyID++ }},
		{"group", func(c *service.ManagedInlineReservation) { c.Owner.GroupID++ }},
		{"account", func(c *service.ManagedInlineReservation) { c.Selection.AccountID++ }},
		{"backend", func(c *service.ManagedInlineReservation) { c.Selection.Identity.BackendID = "another" }},
		{"subject", func(c *service.ManagedInlineReservation) {
			c.Selection.Identity.SubjectFingerprint = "sha256:" + strings.Repeat("f", 64)
		}},
		{"generation", func(c *service.ManagedInlineReservation) { c.Selection.Identity.CredentialGeneration = "epoch-2" }},
		{"request", func(c *service.ManagedInlineReservation) { c.RequestID = "another-request" }},
		{"lease", func(c *service.ManagedInlineReservation) { c.LeaseToken = "another-lease" }},
		{"operation", func(c *service.ManagedInlineReservation) { c.Files[0].Operation.ID = "another-operation" }},
		{"ref", func(c *service.ManagedInlineReservation) { c.Files[0].Descriptor.FileRef = "file-another" }},
		{"hash", func(c *service.ManagedInlineReservation) { c.Files[0].Descriptor.SHA256 = strings.Repeat("f", 64) }},
		{"size", func(c *service.ManagedInlineReservation) { c.Files[0].Descriptor.Bytes++ }},
		{"filename", func(c *service.ManagedInlineReservation) { c.Files[0].Descriptor.Filename = "another.txt" }},
		{"mime", func(c *service.ManagedInlineReservation) { c.Files[0].Descriptor.MIME = "application/pdf" }},
		{"truncatedbatch", func(c *service.ManagedInlineReservation) { c.Files = c.Files[:1] }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			c := cloneInlinePermit(p)
			tc.change(c)
			require.Error(t, repo.StartInline(ctx, c, in.Files[0].FileRef))
			_, err := repo.CompleteInline(ctx, c, in.Files[0].FileRef, inlineResult(p, 0))
			require.Error(t, err)
			require.Error(t, repo.MarkInlineUncertain(ctx, c, in.Files[0].FileRef, "timeout"))
			require.Error(t, repo.AuthorizeInlineGeneration(ctx, c))
			require.Error(t, repo.ReleaseInline(ctx, c))
		})
	}
	for i, f := range p.Files {
		require.NoError(t, repo.StartInline(ctx, p, f.Descriptor.FileRef))
		active, err := repo.CompleteInline(ctx, p, f.Descriptor.FileRef, inlineResult(p, i))
		require.NoError(t, err)
		require.True(t, active)
		if i == 0 {
			require.ErrorIs(t, repo.AuthorizeInlineGeneration(ctx, p), service.ErrManagedFileNotReady)
		}
	}
	require.NoError(t, repo.AuthorizeInlineGeneration(ctx, p))
}

func TestManagedInlineLedgerRejectsExpiryAfterWaitingForRequestLock(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	in.Lease = 200 * time.Millisecond
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `SELECT request_id FROM gateway_inline_requests WHERE request_id=$1 FOR UPDATE`, in.RequestID)
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() { result <- repo.StartInline(ctx, p, in.Files[0].FileRef) }()
	time.Sleep(250 * time.Millisecond)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-result, service.ErrFileLeaseConflict)
	_, err = repo.CompleteInline(ctx, p, in.Files[0].FileRef, inlineResult(p, 0))
	require.ErrorIs(t, err, service.ErrManagedFileNotReady)
}

func TestManagedInlineLedgerCanceledCallerDoesNotEraseLateFact(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	ref := in.Files[0].FileRef
	require.NoError(t, repo.StartInline(ctx, p, ref))
	require.NoError(t, repo.MarkInlineUncertain(ctx, p, ref, "upstream_timeout"))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repo.CompleteInline(canceled, p, ref, inlineResult(p, 0))
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, repo.ReleaseInline(ctx, p))
	active, err := repo.CompleteInline(ctx, p, ref, inlineResult(p, 0))
	require.NoError(t, err)
	require.False(t, active)
}

func TestManagedInlineLedgerRejectsMIMEWithoutTypeAndSubtype(t *testing.T) {
	in := inlineReserveInput(t)
	in.Files[0].MIME = "text"
	inlineCleanup(t, in.RequestID)
	_, err := NewManagedFileRepository(integrationDB).ReserveInline(context.Background(), in)
	require.ErrorIs(t, err, service.ErrInvalidFile)
}

func TestManagedInlineLedgerValidatesWholeBatchBeforeDurableReservation(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	valid := inlineReserveInput(t)
	inlineCleanup(t, valid.RequestID)
	changes := []struct {
		name   string
		change func(*service.ManagedInlineReserveInput)
	}{
		{"empty", func(c *service.ManagedInlineReserveInput) { c.Files = nil }},
		{"count", func(c *service.ManagedInlineReserveInput) {
			c.Files = make([]service.ManagedInlineDescriptor, managedfiles.MaxUniqueFiles+1)
		}},
		{"duplicate", func(c *service.ManagedInlineReserveInput) { c.Files = append(c.Files, c.Files[0]) }},
		{"invalidsecond", func(c *service.ManagedInlineReserveInput) {
			d := c.Files[0]
			d.FileRef = "file-second"
			d.Bytes = 0
			c.Files = append(c.Files, d)
		}},
		{"invalidname", func(c *service.ManagedInlineReserveInput) { c.Files[0].Filename = "../private.txt" }},
		{"negativelease", func(c *service.ManagedInlineReserveInput) { c.Lease = -time.Second }},
		{"submicrolease", func(c *service.ManagedInlineReserveInput) { c.Lease = time.Nanosecond }},
		{"excesslease", func(c *service.ManagedInlineReserveInput) { c.Lease = 31 * time.Minute }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			c.Files = append([]service.ManagedInlineDescriptor(nil), valid.Files...)
			tc.change(&c)
			_, err := repo.ReserveInline(ctx, c)
			require.ErrorIs(t, err, service.ErrInvalidFile)
		})
	}
	// The same request can still reserve after invalid proposals: no partial batch.
	p, err := repo.ReserveInline(ctx, valid)
	require.NoError(t, err)
	require.Len(t, p.Files, 1)
}

func TestManagedInlineLedgerExpiredUploadKeepsConfirmedFactWithoutAuthority(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	in.Lease = 150 * time.Millisecond
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	ref := in.Files[0].FileRef
	require.NoError(t, repo.StartInline(ctx, p, ref))
	time.Sleep(200 * time.Millisecond)
	active, err := repo.CompleteInline(ctx, p, ref, inlineResult(p, 0))
	require.NoError(t, err)
	require.False(t, active)
	require.ErrorIs(t, repo.AuthorizeInlineGeneration(ctx, p), service.ErrFileLeaseConflict)
	changed := inlineResult(p, 0)
	changed.InputTokens++
	_, err = repo.CompleteInline(ctx, p, ref, changed)
	require.ErrorIs(t, err, service.ErrManagedFileBindingConflict)
}

func TestManagedInlineLedgerSchemaProtectsIdentityAndConfirmedFacts(t *testing.T) {
	ctx := context.Background()
	repo := NewManagedFileRepository(integrationDB)
	in := inlineReserveInput(t)
	var before int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM gateway_files WHERE user_id=971`).Scan(&before))
	p, err := repo.ReserveInline(ctx, in)
	require.NoError(t, err)
	inlineCleanup(t, in.RequestID)
	ref := in.Files[0].FileRef
	require.NoError(t, repo.StartInline(ctx, p, ref))
	_, err = repo.CompleteInline(ctx, p, ref, inlineResult(p, 0))
	require.NoError(t, err)
	for _, query := range []string{
		`UPDATE gateway_inline_requests SET main_account_id=975 WHERE request_id=$1`,
		`UPDATE gateway_inline_requests SET lease_until=clock_timestamp()+INTERVAL '1 hour' WHERE request_id=$1`,
		`UPDATE gateway_inline_remote_operations SET sha256=repeat('f',64) WHERE request_id=$1`,
		`UPDATE gateway_inline_remote_operations SET remote_id='file-rewritten' WHERE request_id=$1`,
		`UPDATE gateway_inline_remote_operations SET input_tokens=99 WHERE request_id=$1`,
	} {
		_, err := integrationDB.ExecContext(ctx, query, p.RequestID)
		require.Error(t, err)
	}
	require.NoError(t, repo.AuthorizeInlineGeneration(ctx, p))
	require.NoError(t, repo.ReleaseInline(ctx, p))
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_inline_requests SET released=false WHERE request_id=$1`, p.RequestID)
	require.Error(t, err)
	var after int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM gateway_files WHERE user_id=971`).Scan(&after))
	require.Equal(t, before, after, "inline evidence must not create permanent Files resources")
}
