package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

type managedRecoveryRecorder struct{ calls int }

func (r *managedRecoveryRecorder) ReserveRecovery(_ context.Context, in ManagedFileRecoveryInput) (*ManagedFileReservation, error) {
	r.calls++
	return in.Reservation, nil
}

func managedRecoveryInput() ManagedFileRecoveryInput {
	selection := ManagedFileSelection{AccountID: 71, Identity: managedfiles.Identity{BackendID: "backend-test", SubjectFingerprint: "sha256:" + strings.Repeat("b", 64), CredentialGeneration: "epoch-1"}}
	file := &StoredFile{ID: 19, PublicID: "file-recovery", UserID: 1, GroupID: 2, APIKeyID: 3, Version: 1, State: "ready", Bytes: 3, SHA256: strings.Repeat("a", 64), Filename: "sample.txt", MIME: "text/plain"}
	return ManagedFileRecoveryInput{Reservation: &ManagedFileReservation{Owner: FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, Selection: selection, LeaseToken: "preparation", Files: []ManagedFileBinding{{File: file, Selection: selection, Version: 2, LeaseToken: "preparation", LeaseUntil: time.Now().Add(time.Minute), Operation: ManagedFileOperation{ID: "operation-old", State: "succeeded", RemoteID: "file-native-old", InputTokens: 17}}}}, FileRefs: []string{file.PublicID}}
}

func TestManagedFileRecoveryRequiresFreshAuthorization(t *testing.T) {
	repo := &managedRecoveryRecorder{}
	denied := errors.New("current group permission revoked")
	result, err := ReserveManagedFileRecovery(context.Background(), repo, managedRecoveryInput(), func(context.Context) error { return denied })
	require.ErrorIs(t, err, denied)
	require.Nil(t, result)
	require.Zero(t, repo.calls, "lost permission must prevent every recovery mutation")
}

func TestManagedFileRecoveryAllowsUnsentNonmissingUpload(t *testing.T) {
	in := managedRecoveryInput()
	unchanged := in.Reservation.Files[0]
	copy := *unchanged.File
	copy.ID, copy.PublicID = 20, "file-new-upload"
	unchanged.File = &copy
	unchanged.Operation = ManagedFileOperation{ID: "operation-unsent", State: "reserved"}
	in.Reservation.Files = append(in.Reservation.Files, unchanged)
	repo := &managedRecoveryRecorder{}
	authorized := 0
	result, err := ReserveManagedFileRecovery(context.Background(), repo, in, func(context.Context) error {
		authorized++
		return nil
	})
	require.NoError(t, err, "a missing existing copy must not reject an unrelated unsent upload")
	require.Same(t, in.Reservation, result)
	require.Equal(t, 1, authorized)
	require.Equal(t, 1, repo.calls)
}

func TestManagedFileRecoveryRejectsDuplicatePermitBeforeAuthorization(t *testing.T) {
	in := managedRecoveryInput()
	in.Reservation.Files = append(in.Reservation.Files, in.Reservation.Files[0])
	repo := &managedRecoveryRecorder{}
	authorized := 0
	result, err := ReserveManagedFileRecovery(context.Background(), repo, in, func(context.Context) error {
		authorized++
		return nil
	})
	require.Error(t, err)
	require.Nil(t, result)
	require.Zero(t, authorized)
	require.Zero(t, repo.calls)
}

func TestManagedFileRecoveryRechecksCancellationAfterAuthorization(t *testing.T) {
	repo := &managedRecoveryRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := ReserveManagedFileRecovery(ctx, repo, managedRecoveryInput(), func(context.Context) error {
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Zero(t, repo.calls)
}

func TestManagedFileRecoveryRejectsInvalidInputWithoutAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ManagedFileRecoveryInput)
	}{
		{"missing permit", func(in *ManagedFileRecoveryInput) { in.Reservation = nil }},
		{"empty batch", func(in *ManagedFileRecoveryInput) { in.Reservation.Files = nil }},
		{"invalid current key", func(in *ManagedFileRecoveryInput) { in.Reservation.Owner.APIKeyID = 0 }},
		{"invalid backend identity", func(in *ManagedFileRecoveryInput) { in.Reservation.Selection.Identity.BackendID = "" }},
		{"missing lease", func(in *ManagedFileRecoveryInput) { in.Reservation.LeaseToken = "" }},
		{"empty missing subset", func(in *ManagedFileRecoveryInput) { in.FileRefs = nil }},
		{"duplicate missing subset", func(in *ManagedFileRecoveryInput) { in.FileRefs = []string{"file-recovery", "file-recovery"} }},
		{"unknown missing file", func(in *ManagedFileRecoveryInput) { in.FileRefs = []string{"file-other"} }},
		{"nil file", func(in *ManagedFileRecoveryInput) { in.Reservation.Files[0].File = nil }},
		{"different frozen backend", func(in *ManagedFileRecoveryInput) { in.Reservation.Files[0].Selection.AccountID++ }},
		{"different lease", func(in *ManagedFileRecoveryInput) { in.Reservation.Files[0].LeaseToken = "another-request" }},
		{"missing operation", func(in *ManagedFileRecoveryInput) { in.Reservation.Files[0].Operation.ID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := managedRecoveryInput()
			tc.change(&in)
			repo := &managedRecoveryRecorder{}
			authorized := 0
			result, err := ReserveManagedFileRecovery(context.Background(), repo, in, func(context.Context) error { authorized++; return nil })
			require.Error(t, err)
			require.Nil(t, result)
			require.Zero(t, authorized)
			require.Zero(t, repo.calls)
		})
	}
}

func TestManagedFileRecoveryRequiresAuthorizationAndRepository(t *testing.T) {
	in := managedRecoveryInput()
	repo := &managedRecoveryRecorder{}
	_, err := ReserveManagedFileRecovery(context.Background(), repo, in, nil)
	require.ErrorIs(t, err, ErrManagedFileNotReady)
	require.Zero(t, repo.calls)
	_, err = ReserveManagedFileRecovery(context.Background(), nil, in, func(context.Context) error { return nil })
	require.ErrorIs(t, err, ErrManagedFileNotReady)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	authorized := false
	_, err = ReserveManagedFileRecovery(ctx, repo, in, func(context.Context) error { authorized = true; return nil })
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, authorized)
	require.Zero(t, repo.calls)
}
