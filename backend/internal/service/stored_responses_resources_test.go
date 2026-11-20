package service

import (
	"context"
	"errors"
	"testing"
)

func TestStoredResponseResourcesEnabledIsNilSafeAndFollowsConfiguration(t *testing.T) {
	var absent *StoredResponseService
	if absent.Enabled() {
		t.Fatal("nil service enabled resources")
	}
	for _, enabled := range []bool{false, true} {
		svc := &StoredResponseService{cfg: StoredResponseConfig{Enabled: enabled}}
		if got := svc.Enabled(); got != enabled {
			t.Fatalf("Enabled() = %t; want %t", got, enabled)
		}
	}
}

func TestStoredResponseManagementModelPreservesOwnerGroupAndReplacementKeyScope(t *testing.T) {
	db := &storedResponseManagementDB{model: "client-model"}
	svc := &StoredResponseService{repo: db, cfg: StoredResponseConfig{Enabled: true}}
	for _, keyID := range []int64{2, 22} {
		owner := StoredResponseOwner{UserID: 1, APIKeyID: keyID, GroupID: 3}
		model, err := svc.GetManagementModel(context.Background(), owner, "response-id")
		if err != nil || model != "client-model" {
			t.Fatalf("replacement key %d: model=%q err=%v", keyID, model, err)
		}
		if db.userID != 1 || db.groupID != 3 || db.id != "response-id" {
			t.Fatalf("wrong management scope: %+v", db)
		}
	}
	if db.calls != 2 {
		t.Fatalf("metadata queries = %d; want 2", db.calls)
	}
}

func TestStoredResponseManagementModelRejectsUnavailableOrInvalidOwnersBeforeLookup(t *testing.T) {
	valid := StoredResponseOwner{UserID: 1, APIKeyID: 2, GroupID: 3}
	for _, tc := range []struct {
		name  string
		svc   *StoredResponseService
		owner StoredResponseOwner
		want  error
	}{
		{name: "nil service", owner: valid, want: ErrStoredResponsesDisabled},
		{name: "disabled", svc: &StoredResponseService{}, owner: valid, want: ErrStoredResponsesDisabled},
		{name: "nil repository", svc: &StoredResponseService{cfg: StoredResponseConfig{Enabled: true}}, owner: valid, want: ErrStoredResponseNotFound},
		{name: "missing user", owner: StoredResponseOwner{APIKeyID: 2, GroupID: 3}, want: ErrStoredResponseNotFound},
		{name: "missing key", owner: StoredResponseOwner{UserID: 1, GroupID: 3}, want: ErrStoredResponseNotFound},
		{name: "missing group", owner: StoredResponseOwner{UserID: 1, APIKeyID: 2}, want: ErrStoredResponseNotFound},
		{name: "negative user", owner: StoredResponseOwner{UserID: -1, APIKeyID: 2, GroupID: 3}, want: ErrStoredResponseNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &storedResponseManagementDB{model: "client-model"}
			svc := tc.svc
			if svc == nil && tc.name != "nil service" {
				svc = &StoredResponseService{repo: db, cfg: StoredResponseConfig{Enabled: true}}
			}
			_, err := svc.GetManagementModel(context.Background(), tc.owner, "response-id")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v; want %v", err, tc.want)
			}
			if db.calls != 0 {
				t.Fatal("unavailable/invalid owner reached metadata repository")
			}
		})
	}
}

func TestStoredResponseManagementModelPropagatesRepositoryErrorsAndFailsClosedForEmptyModel(t *testing.T) {
	failure := errors.New("synthetic repository failure")
	owner := StoredResponseOwner{UserID: 1, APIKeyID: 2, GroupID: 3}
	for _, tc := range []struct {
		name, model string
		err         error
	}{
		{name: "repository failure", err: failure},
		{name: "not found", err: ErrStoredResponseNotFound},
		{name: "empty model"},
		{name: "whitespace model", model: " \t\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &storedResponseManagementDB{model: tc.model, err: tc.err}
			svc := &StoredResponseService{repo: db, cfg: StoredResponseConfig{Enabled: true}}
			model, err := svc.GetManagementModel(context.Background(), owner, "response-id")
			if model != "" || err == nil {
				t.Fatalf("unsafe model lookup: model=%q err=%v", model, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("lost repository error: %v", err)
			}
			if tc.err == nil && (errors.Is(err, ErrStoredResponseInvalid) || errors.Is(err, ErrStoredResponseNotFound)) {
				t.Fatalf("storage inconsistency incorrectly attributed to client: %v", err)
			}
		})
	}
}

// This adapter exercises only the management boundary. Resource authorization
// and tombstone persistence require the actual middleware and PostgreSQL tests.
type storedResponseManagementDB struct {
	StoredResponseRepository
	model           string
	err             error
	userID, groupID int64
	id              string
	calls           int
}

func (d *storedResponseManagementDB) GetManagementModel(_ context.Context, userID, groupID int64, id string) (string, error) {
	d.userID, d.groupID, d.id = userID, groupID, id
	d.calls++
	return d.model, d.err
}
