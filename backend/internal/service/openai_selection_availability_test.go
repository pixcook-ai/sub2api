package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestOpenAISelectionAvailability_OnlyRateLimitCooldown(t *testing.T) {
	groupID := int64(17)
	firstReset := time.Now().Add(10 * time.Second)
	secondReset := firstReset.Add(20 * time.Second)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &secondReset},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &firstReset},
	}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}}
	result := svc.DiagnoseOpenAISelectionAvailability(context.Background(), OpenAIAccountScheduleRequest{
		GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny,
	})
	require.True(t, result.HasModelSupport)
	require.Equal(t, firstReset, result.RetryAt, "all configured matching accounts are blocked only by rate limit, so callers can preserve 429")
	require.Equal(t, &secondReset, accounts[0].RateLimitResetAt, "diagnosis must not clear the source account cooldown")
}

func TestOpenAISelectionAvailability_UnknownRuntimeBlockRetainsFallback(t *testing.T) {
	groupID := int64(17)
	reset := time.Now().Add(time.Minute)
	account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}}}
	svc.openaiAccountRuntimeBlockUntil.Store(account.ID, "unknown block")
	result := svc.DiagnoseOpenAISelectionAvailability(context.Background(), OpenAIAccountScheduleRequest{
		GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny,
	})
	require.Zero(t, result.RetryAt, "unrecognized runtime state is not proof of a rate-limit-only failure")
	stored, exists := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, exists)
	require.Equal(t, "unknown block", stored, "diagnosis must not clear unknown state")
}

type selectionAvailabilityRepo struct {
	AccountRepository
	accounts        []Account
	calls           int
	err             error
	waitForDeadline bool
	hasDeadline     bool
}

func (r *selectionAvailabilityRepo) ListModelAvailabilityCandidates(ctx context.Context, _ *int64, _ []string, _ bool) ([]Account, error) {
	r.calls++
	_, r.hasDeadline = ctx.Deadline()
	if r.waitForDeadline {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.accounts, r.err
}

func TestOpenAISelectionAvailability_NonRateReasonsRetainFallback(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Account, *OpenAIAccountScheduleRequest, *OpenAIGatewayService)
	}{
		{"mixed healthy account", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) { a.RateLimitResetAt = nil }},
		{"overload", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			a.OverloadUntil = a.RateLimitResetAt
		}},
		{"temporary pause", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			a.TempUnschedulableUntil = a.RateLimitResetAt
		}},
		{"expired", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			past := time.Now().Add(-time.Hour)
			a.AutoPauseOnExpired = true
			a.ExpiresAt = &past
		}},
		{"disabled", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) { a.Status = StatusDisabled }},
		{"scheduling off", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) { a.Schedulable = false }},
		{"quota", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			a.Extra = map[string]any{"quota_limit": 1.0, "quota_used": 1.0}
		}},
		{"privacy", func(_ *Account, r *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) { r.RequirePrivacySet = true }},
		{"compact", func(_ *Account, r *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) { r.RequireCompact = true }},
		{"transport", func(_ *Account, r *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			r.RequiredTransport = OpenAIUpstreamTransport("unknown")
		}},
		{"capability", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			a.Extra = map[string]any{"openai_responses_supported": false}
		}},
		{"model cooldown", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			setAccountModelRateLimitSnapshot(a, "gpt-6.1-sol", *a.RateLimitResetAt, "model_limit", time.Now())
		}},
		{"shadow policy unknown", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			id := int64(9)
			a.ParentAccountID = &id
		}},
		{"oauth policy unknown", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) { a.Type = AccountTypeOAuth }},
		{"proxy policy unknown", func(a *Account, _ *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			id := int64(9)
			a.ProxyID = &id
		}},
		{"strong response binding", func(_ *Account, r *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			r.PreviousResponseID = "resp_owner"
		}},
		{"excluded", func(a *Account, r *OpenAIAccountScheduleRequest, _ *OpenAIGatewayService) {
			r.ExcludedIDs = map[int64]struct{}{a.ID: {}}
		}},
		{"longer unexplained runtime block", func(a *Account, _ *OpenAIAccountScheduleRequest, s *OpenAIGatewayService) {
			s.openaiAccountRuntimeBlockUntil.Store(a.ID, a.RateLimitResetAt.Add(time.Hour))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(17)
			reset := time.Now().Add(time.Minute)
			base := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}
			other := base
			other.ID = 2
			req := OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny}
			repo := &selectionAvailabilityRepo{}
			svc := &OpenAIGatewayService{accountRepo: repo}
			tc.change(&other, &req, svc)
			repo.accounts = []Account{base, other}
			result := svc.DiagnoseOpenAISelectionAvailability(context.Background(), req)
			require.True(t, result.HasModelSupport)
			require.Zero(t, result.RetryAt, "one non-rate or unknown cause makes the rate-only claim unproven")
			require.Equal(t, 1, repo.calls)
		})
	}
}

func TestOpenAISelectionAvailability_EmptyUnsupportedAndLookupFailure(t *testing.T) {
	groupID := int64(17)
	req := OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "public-model", RequiredTransport: OpenAIUpstreamTransportAny}
	t.Run("removed group or empty pool", func(t *testing.T) {
		repo := &selectionAvailabilityRepo{}
		result := (&OpenAIGatewayService{accountRepo: repo}).DiagnoseOpenAISelectionAvailability(context.Background(), req)
		require.False(t, result.HasAccountsInPool)
		require.Zero(t, result.RetryAt)
	})
	t.Run("unsupported model", func(t *testing.T) {
		reset := time.Now().Add(time.Minute)
		repo := &selectionAvailabilityRepo{accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset, Credentials: map[string]any{"model_mapping": map[string]any{"other-model": "upstream"}}}}}
		result := (&OpenAIGatewayService{accountRepo: repo}).DiagnoseOpenAISelectionAvailability(context.Background(), req)
		require.True(t, result.HasAccountsInPool)
		require.False(t, result.HasModelSupport)
		require.Zero(t, result.RetryAt)
	})
	t.Run("lookup error", func(t *testing.T) {
		repo := &selectionAvailabilityRepo{err: errors.New("database unavailable")}
		result := (&OpenAIGatewayService{accountRepo: repo}).DiagnoseOpenAISelectionAvailability(context.Background(), req)
		require.True(t, result.HasModelSupport, "lookup failure must retain 503 rather than fabricate a model or rate-limit verdict")
		require.Zero(t, result.RetryAt)
		require.True(t, repo.hasDeadline, "the error-path repository query must be bounded")
		require.Equal(t, 1, repo.calls)
	})
	t.Run("caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		repo := &selectionAvailabilityRepo{waitForDeadline: true}
		started := time.Now()
		result := (&OpenAIGatewayService{accountRepo: repo}).DiagnoseOpenAISelectionAvailability(ctx, req)
		require.True(t, result.HasModelSupport)
		require.Zero(t, result.RetryAt)
		require.Less(t, time.Since(started), time.Second)
	})
}

func TestOpenAISelectionAvailability_DoesNotMutateRuntimeState(t *testing.T) {
	for _, activeModelBlock := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale model block", true: "active model block"}[activeModelBlock], func(t *testing.T) {
			groupID := int64(17)
			now := time.Now()
			reset := now.Add(time.Minute)
			account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}
			svc := &OpenAIGatewayService{accountRepo: &selectionAvailabilityRepo{accounts: []Account{account}}}
			svc.BlockAccountScheduling(&account, reset, "429_fallback")
			generation := svc.openaiAccountRuntimeBlockSequence.Load()
			runtimeValue, _ := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
			state := svc.getOpenAIAccountModelTransientState()
			key := openAIAccountModelKey{AccountID: account.ID, Model: "gpt-6.1-sol"}
			entry := openAIAccountModelTransientEntry{failureStreak: 2, lastFailure: now.Add(-time.Hour), blockUntil: reset, lastTouched: now.Add(-time.Hour)}
			if activeModelBlock {
				entry.lastFailure = now
			}
			state.entries[key] = entry
			result := svc.DiagnoseOpenAISelectionAvailability(context.Background(), OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny})
			if activeModelBlock {
				require.Zero(t, result.RetryAt)
			} else {
				require.Equal(t, reset, result.RetryAt)
			}
			require.Equal(t, generation, svc.openaiAccountRuntimeBlockSequence.Load())
			after, _ := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
			require.Equal(t, runtimeValue, after)
			require.Equal(t, entry, state.entries[key], "neither stale cleanup nor lastTouched may run on diagnosis")
			require.Equal(t, &reset, account.RateLimitResetAt)
		})
	}
}

func TestOpenAISelectionAvailability_ImplicitRequestPoliciesRetainFallback(t *testing.T) {
	for _, name := range []string{"guardian context", "global quota default", "channel billing restriction"} {
		t.Run(name, func(t *testing.T) {
			groupID := int64(17)
			reset := time.Now().Add(time.Minute)
			account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}
			repo := &selectionAvailabilityRepo{accounts: []Account{account}}
			svc := &OpenAIGatewayService{accountRepo: repo}
			ctx := context.Background()
			switch name {
			case "guardian context":
				ctx = context.WithValue(ctx, openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent"})
			case "global quota default":
				svc.settingService = &SettingService{}
				svc.settingService.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.8})
				repo.accounts[0].Extra = map[string]any{"codex_5h_used_percent": 90.0, "codex_5h_reset_at": reset.Format(time.RFC3339)}
			case "channel billing restriction":
				svc.channelService = &ChannelService{}
				svc.channelService.cache.Store(populateChannelCache([]Channel{{ID: 1, GroupIDs: []int64{groupID}, RestrictModels: true, BillingModelSource: BillingModelSourceRequested}}, map[int64]string{groupID: PlatformOpenAI}))
			}
			result := svc.DiagnoseOpenAISelectionAvailability(ctx, OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny})
			require.Zero(t, result.RetryAt, "implicit selection policies must be included or conservatively decline proof")
		})
	}
}

type selectionColdSettingsRepo struct {
	SettingRepository
	calls int
}

func (r *selectionColdSettingsRepo) GetValue(ctx context.Context, _ string) (string, error) {
	r.calls++
	<-ctx.Done()
	return "", ctx.Err()
}

func TestOpenAISelectionAvailability_ColdPolicyCacheDoesNotLoad(t *testing.T) {
	cached, _ := accountSchedulingThresholdsCache.Load().(*cachedAccountSchedulingThresholds)
	t.Cleanup(func() {
		if cached == nil {
			cached = &cachedAccountSchedulingThresholds{}
		}
		accountSchedulingThresholdsCache.Store(cached)
	})
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	groupID := int64(17)
	reset := time.Now().Add(time.Minute)
	account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}
	settingsRepo := &selectionColdSettingsRepo{}
	svc := &OpenAIGatewayService{accountRepo: &selectionAvailabilityRepo{accounts: []Account{account}}, rateLimitService: &RateLimitService{settingService: &SettingService{settingRepo: settingsRepo}}}
	started := time.Now()
	result := svc.DiagnoseOpenAISelectionAvailability(context.Background(), OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny})
	require.Zero(t, result.RetryAt)
	require.Zero(t, settingsRepo.calls, "diagnosis must not cold-load policy through an independent timeout")
	require.Less(t, time.Since(started), time.Second)
}

func TestOpenAISelectionAvailability_UsesWarmObservedPolicies(t *testing.T) {
	prior, _ := accountSchedulingThresholdsCache.Load().(*cachedAccountSchedulingThresholds)
	t.Cleanup(func() {
		if prior == nil {
			prior = &cachedAccountSchedulingThresholds{}
		}
		accountSchedulingThresholdsCache.Store(prior)
	})
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{thresholds: map[string]int{PlatformOpenAI: 100}, expiresAt: time.Now().Add(time.Minute).UnixNano(), complete: true})
	groupID := int64(17)
	reset := time.Now().Add(time.Minute)
	repo := &selectionAvailabilityRepo{accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}}}
	settings := &SettingService{}
	settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{})
	channels := &ChannelService{}
	snapshot := populateChannelCache(nil, map[int64]string{groupID: PlatformOpenAI})
	channels.cache.Store(snapshot)
	svc := &OpenAIGatewayService{accountRepo: repo, settingService: settings, channelService: channels, schedulerSnapshot: &SchedulerSnapshotService{}, rateLimitService: &RateLimitService{settingService: settings}}
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true})
	result := svc.DiagnoseOpenAISelectionAvailability(ctx, OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny})
	require.Equal(t, reset, result.RetryAt, "a normally wired service with known unrestricted policies must retain 429")
	require.Equal(t, 1, repo.calls)
	require.Same(t, snapshot, channels.cache.Load(), "diagnosis must not rebuild or invalidate observed channel state")
}

type selectionErrorSettingsRepo struct {
	SettingRepository
	calls int
}

func (r *selectionErrorSettingsRepo) GetValue(context.Context, string) (string, error) {
	r.calls++
	return "", errors.New("settings database unavailable")
}

func TestOpenAISelectionAvailability_FreshErrorPolicyCachesRemainUnknown(t *testing.T) {
	prior, _ := accountSchedulingThresholdsCache.Load().(*cachedAccountSchedulingThresholds)
	t.Cleanup(func() {
		if prior == nil {
			prior = &cachedAccountSchedulingThresholds{}
		}
		accountSchedulingThresholdsCache.Store(prior)
	})
	for _, kind := range []string{"quota", "threshold"} {
		t.Run(kind, func(t *testing.T) {
			groupID := int64(17)
			reset := time.Now().Add(time.Minute)
			repo := &selectionAvailabilityRepo{accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}}}
			settingsRepo := &selectionErrorSettingsRepo{}
			settings := &SettingService{settingRepo: settingsRepo}
			svc := &OpenAIGatewayService{accountRepo: repo}
			if kind == "quota" {
				settings.WarmOpenAIQuotaAutoPauseSettings(context.Background())
				svc.settingService = settings
			} else {
				accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
				settings.GetAccountSchedulingThresholds(context.Background())
				svc.rateLimitService = &RateLimitService{settingService: settings}
			}
			result := svc.DiagnoseOpenAISelectionAvailability(context.Background(), OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-6.1-sol", RequiredCapability: OpenAIEndpointCapabilityResponses, RequiredTransport: OpenAIUpstreamTransportAny})
			require.Zero(t, result.RetryAt, "a fresh fallback cache from a failed lookup is still unproven policy")
			require.Equal(t, 1, settingsRepo.calls, "diagnosis must not retry the failed policy query")
		})
	}
}
