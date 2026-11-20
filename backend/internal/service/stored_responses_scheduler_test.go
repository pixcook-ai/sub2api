package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type storedResponseRouteAccountRepo struct {
	AccountRepository
	accounts  []Account
	listCalls int
	getCalls  int
	current   *Account
}

func (r *storedResponseRouteAccountRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	r.listCalls++
	return r.accounts, nil
}

func TestStoredResponsesRouteRejectsMixedPoolDespiteTransientState(t *testing.T) {
	groupID := int64(14100)
	local := storedResponsesTestAccount(14101, groupID)
	ordinary := storedResponsesTestAccount(14102, groupID)
	ordinary.Credentials = map[string]any{"api_key": "sk-native-test", "base_url": "https://native.example", "openai_capabilities": []string{"chat_completions", "responses"}}
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	ordinary.RateLimitResetAt, ordinary.OverloadUntil, ordinary.ExpiresAt = &future, &future, &past
	repo := &storedResponseRouteAccountRepo{accounts: []Account{local, ordinary}}
	svc := &OpenAIGatewayService{accountRepo: repo, cfg: &config.Config{}}
	_, err := svc.ResolveBPSStoredResponseRoute(context.Background(), &groupID, "fixture-text", PlatformOpenAI)
	require.Error(t, err)
	require.Equal(t, 1, repo.listCalls)
}

func (r *storedResponseRouteAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.getCalls++
	return r.current, nil
}

func TestStoredResponsesRouteRechecksOwningRowAndCredentialFence(t *testing.T) {
	groupID := int64(14200)
	selected := storedResponsesTestAccount(14201, groupID)
	current := storedResponsesTestAccount(14201, groupID)
	repo := &storedResponseRouteAccountRepo{current: &current}
	svc := &OpenAIGatewayService{accountRepo: repo, cfg: &config.Config{}}
	latest, err := svc.RecheckBPSStoredResponseRoute(context.Background(), &groupID, &selected, "fixture-text", BPSStoredResponseRoute{Local: true})
	require.NoError(t, err)
	require.Same(t, &current, latest)
	require.Equal(t, 1, repo.getCalls)
	current.Credentials["api_key"] = "sk-rotated-test"
	_, err = svc.RecheckBPSStoredResponseRoute(context.Background(), &groupID, &selected, "fixture-text", BPSStoredResponseRoute{Local: true})
	require.ErrorIs(t, err, ErrBPSStoredResponseRoute)
	require.Equal(t, 2, repo.getCalls)
}

func TestStoredResponsesRouteFenceRejectsStorageDomainRotation(t *testing.T) {
	groupID := int64(14200)
	selected := storedResponsesTestAccount(14201, groupID)
	selected.Credentials = map[string]any{"api_key": "sk-native-test", "base_url": "https://native.example", "openai_capabilities": []string{"chat_completions", "responses"}}
	current := storedResponsesTestAccount(14201, groupID)
	repo := &storedResponseRouteAccountRepo{current: &current}
	svc := &OpenAIGatewayService{accountRepo: repo, cfg: &config.Config{}}
	_, err := svc.RecheckBPSStoredResponseRoute(context.Background(), &groupID, &selected, "fixture-text", BPSStoredResponseRoute{})
	require.ErrorIs(t, err, ErrBPSStoredResponseRoute)
	require.Equal(t, 1, repo.getCalls)
}

func TestStoredResponsesRouteRecheckPreservesSimpleModePoolSemantics(t *testing.T) {
	requestGroupID := int64(14200)
	current := storedResponsesTestAccount(14201, 14299)
	repo := &storedResponseRouteAccountRepo{current: &current}
	svc := &OpenAIGatewayService{accountRepo: repo, cfg: &config.Config{RunMode: config.RunModeSimple}}
	latest, err := svc.RecheckBPSStoredResponseAccount(context.Background(), &requestGroupID, current.ID, "fixture-text")
	require.NoError(t, err)
	require.Same(t, &current, latest)
	svc.cfg.RunMode = "standard"
	_, err = svc.RecheckBPSStoredResponseAccount(context.Background(), &requestGroupID, current.ID, "fixture-text")
	require.ErrorIs(t, err, ErrBPSStoredResponseRoute)
}

func TestStoredResponsesRouteClassifiesPersistentDomains(t *testing.T) {
	groupID := int64(14200)
	local := storedResponsesTestAccount(14201, groupID)
	ordinary := storedResponsesTestAccount(14202, groupID)
	ordinary.Credentials = map[string]any{"api_key": "sk-native-test", "base_url": "https://native.example", "openai_capabilities": []string{"chat_completions", "responses"}}
	malformed := storedResponsesTestAccount(14203, groupID)
	delete(malformed.Credentials, "bps_responses_service_key")
	for _, tc := range []struct {
		name           string
		accounts       []Account
		local, wantErr bool
	}{
		{"local", []Account{local}, true, false},
		{"ordinary", []Account{ordinary}, false, false},
		{"mixed", []Account{local, ordinary}, false, true},
		{"malformed", []Account{malformed}, false, true},
		{"empty", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{accountRepo: &storedResponseRouteAccountRepo{accounts: tc.accounts}, cfg: &config.Config{}}
			route, err := svc.ResolveBPSStoredResponseRoute(context.Background(), &groupID, "fixture-text", PlatformOpenAI)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.local, route.Local)
			}
		})
	}
}

func TestStoredResponsesOrdinaryRouteKeepsExpirationAutoPausePolicy(t *testing.T) {
	groupID := int64(14200)
	current := storedResponsesTestAccount(14201, groupID)
	current.Credentials = map[string]any{"api_key": "sk-native-test", "base_url": "https://native.example", "openai_capabilities": []string{"chat_completions", "responses"}}
	past := time.Now().Add(-time.Hour)
	current.ExpiresAt = &past
	current.AutoPauseOnExpired = false
	repo := &storedResponseRouteAccountRepo{current: &current}
	svc := &OpenAIGatewayService{accountRepo: repo, cfg: &config.Config{}}
	_, err := svc.RecheckBPSStoredResponseRoute(context.Background(), &groupID, &current, "fixture-text", BPSStoredResponseRoute{})
	require.NoError(t, err, "ordinary account with auto-pause disabled retains its existing eligibility")
	current.AutoPauseOnExpired = true
	_, err = svc.RecheckBPSStoredResponseRoute(context.Background(), &groupID, &current, "fixture-text", BPSStoredResponseRoute{})
	require.ErrorIs(t, err, ErrBPSStoredResponseRoute)
}
