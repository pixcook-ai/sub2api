package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type storedResponseBudgetRepository struct {
	StoredResponseRepository
	limits           StoredResponseLimits
	retention, lease time.Duration
}

var errStoredResponseBudgetProbe = errors.New("reservation budget probe")

func (r *storedResponseBudgetRepository) Reserve(_ context.Context, _ *StoredResponse, l StoredResponseLimits, retention, lease time.Duration) error {
	r.limits, r.retention, r.lease = l, retention, lease
	return errStoredResponseBudgetProbe
}

func TestProvideStoredResponseServicePreservesConfiguredReservationBudgets(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.StoredResponses = config.StoredResponsesConfig{Enabled: true, RetentionDays: 7, LeaseSeconds: 30, MaxResponseBytes: 4096, UserMaxRows: 10, UserMaxBytes: 8192, GlobalMaxRows: 20, GlobalMaxBytes: 16384}
	repo := &storedResponseBudgetRepository{}
	svc, err := ProvideStoredResponseService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, ResolvedModel: "fixture", Scope: "fixture-v1"})
	if !errors.Is(err, errStoredResponseBudgetProbe) {
		t.Fatalf("reservation not reached: %v", err)
	}
	want := StoredResponseLimits{MaxResponseBytes: 4096, UserMaxRows: 10, UserMaxBytes: 8192, GlobalMaxRows: 20, GlobalMaxBytes: 16384}
	if repo.limits != want || repo.retention != 7*24*time.Hour || repo.lease != 30*time.Second {
		t.Fatalf("reservation budgets changed: %+v %s %s", repo.limits, repo.retention, repo.lease)
	}
}
