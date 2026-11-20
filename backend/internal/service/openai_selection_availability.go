package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

const openAISelectionDiagnosisTimeout = 500 * time.Millisecond

// OpenAISelectionAvailability keeps model configuration separate from a proven
// account-rate-limit-only failure. A zero RetryAt retains the ordinary fallback.
type OpenAISelectionAvailability struct {
	ModelAvailabilityDiagnosis
	RetryAt time.Time
}

// DiagnoseOpenAISelectionAvailability diagnoses a failed HTTP Responses selection.
// It reuses the configured-pool query needed for model attribution, with a bounded
// deadline. Admission and cooldown state are never changed by this error path.
func (s *OpenAIGatewayService) DiagnoseOpenAISelectionAvailability(ctx context.Context, req OpenAIAccountScheduleRequest) OpenAISelectionAvailability {
	fallback := OpenAISelectionAvailability{ModelAvailabilityDiagnosis: ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}}
	if s == nil || s.accountRepo == nil || strings.TrimSpace(req.RequestedModel) == "" {
		return fallback
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, openAISelectionDiagnosisTimeout)
	defer cancel()
	accounts, err := s.listOpenAIModelAvailabilityCandidates(ctx, req.GroupID, req.Platform)
	if err != nil || ctx.Err() != nil {
		return fallback
	}
	result := OpenAISelectionAvailability{ModelAvailabilityDiagnosis: diagnoseOpenAIModelAvailabilityCandidates(accounts, req.RequestedModel)}
	// Only the ordinary OpenAI HTTP Responses policy is proven here. Complex
	// routing/binding and oversized pools retain the existing availability error.
	if NormalizeOpenAICompatiblePlatform(req.Platform) != PlatformOpenAI || len(accounts) > 256 ||
		req.RequireCompact || req.RequiredImageCapability != "" || req.PreviousResponseID != "" ||
		len(req.ExcludedIDs) != 0 || req.GuardianParentAccountID != 0 ||
		(req.RequiredTransport != OpenAIUpstreamTransportAny && req.RequiredTransport != OpenAIUpstreamTransportHTTPSSE) ||
		OpenAIImageGenerationIntentFromContext(ctx) {
		return result
	}
	ctx, thresholds, known := s.openAISelectionPolicySnapshot(ctx, &req)
	if !known {
		return result
	}
	checker := &defaultOpenAIAccountScheduler{service: s}
	now := time.Now()
	var earliest time.Time
	for i := range accounts {
		account := &accounts[i]
		if !account.IsModelSupported(req.RequestedModel) {
			continue
		}
		if ctx.Err() != nil || account.RateLimitResetAt == nil || !now.Before(*account.RateLimitResetAt) {
			return result
		}
		// These account forms have additional stateful or external admission
		// policies. An unproven reason must never become a rate-limit verdict.
		if account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey || account.ProxyID != nil ||
			account.UsesGatewayPool() || account.IsShadow() || ResolveOpenAIAutoResetCreditConfig(account).Enabled || !s.openAISelectionAccountRuntimeKnown(account) {
			return result
		}
		candidate := *account
		candidate.RateLimitResetAt = nil
		if !candidate.IsSchedulable() {
			return result
		}
		if compatible, _ := checker.isAccountRequestCompatibleReasonWithChecks(ctx, &candidate, req, openAIAccountCompatibilityChecks{runtimeBlocked: s.openAISelectionModelRuntimeBlocked}); !compatible {
			return result
		}
		// Unlike ApplyAccountSchedulingThreshold, this is a pure evaluation:
		// no temporary pause, runtime block, notification, or repository write.
		if EvaluateAccountSchedulingThreshold(&candidate, thresholds, now).ShouldPause {
			return result
		}
		if earliest.IsZero() || account.RateLimitResetAt.Before(earliest) {
			earliest = *account.RateLimitResetAt
		}
	}
	if ctx.Err() == nil && time.Now().Before(earliest) {
		result.RetryAt = earliest
	}
	return result
}

func (s *OpenAIGatewayService) openAISelectionModelRuntimeBlocked(account *Account, model string) bool {
	// Account-level runtime blocks are authorized by persisted cooldowns. The
	// counterfactual candidate has only RateLimitResetAt removed and passed
	// IsSchedulable, so that ordinary block would be cleared by admission. Do not
	// invoke its mutating clear/CAS path here; preserve model-scoped blocks.
	state := s.getOpenAIAccountModelTransientState()
	return state.peekBlocked(account.ID, openAIAccountModelTransientModel(canonicalOpenAIAccountSchedulingModel(account, model)), time.Now())
}

// Read only the account block deadline; the normal peek helper expires entries
// and advances generations. Unknown or independently longer blocks retain 503.
func (s *OpenAIGatewayService) openAISelectionAccountRuntimeKnown(account *Account) bool {
	value, exists := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !exists {
		return true
	}
	until, ok := value.(time.Time)
	return ok && !until.IsZero() && !until.After(*account.RateLimitResetAt)
}

// Snapshot only already-observed policy. Ordinary getters may start independent
// refreshes or record vetoes; an error response must never do that work. Missing,
// stale, restrictive, or complex policy declines the new rate-limit proof.
func (s *OpenAIGatewayService) openAISelectionPolicySnapshot(ctx context.Context, req *OpenAIAccountScheduleRequest) (context.Context, map[string]int, bool) {
	if _, affinity := openAIGuardianParentAffinityFromContext(ctx); affinity {
		return ctx, nil, false
	}
	if gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate); gate != nil {
		return ctx, nil, false
	}
	if req.GroupID != nil {
		group, ok := ctx.Value(ctxkey.Group).(*Group)
		if ok && IsGroupContextValid(group) && group.ID == *req.GroupID {
			if group.ProfitControlEnabled {
				return ctx, nil, false
			}
			req.RequirePrivacySet = req.RequirePrivacySet || group.RequirePrivacySet
		} else if s.schedulerSnapshot != nil {
			return ctx, nil, false
		}
	}
	now := time.Now()
	if s.settingService != nil {
		cached, _ := s.settingService.openAIQuotaAutoPauseSettingsCache.Load().(*cachedOpenAIQuotaAutoPauseSettings)
		if cached == nil || !cached.complete || now.UnixNano() >= cached.expiresAt {
			return ctx, nil, false
		}
		ctx = withOpenAIQuotaAutoPauseSettings(ctx, cached.settings)
	}
	if s.channelService != nil && req.GroupID != nil {
		cached, _ := s.channelService.cache.Load().(*channelCache)
		if cached == nil || !cached.complete || now.Sub(cached.loadedAt) >= channelCacheTTL {
			return ctx, nil, false
		}
		// Reject any restrictive channel, including billing-model restrictions
		// evaluated before the per-account upstream-model gate. The shared
		// predicate may omit its channel callback only after this proof.
		if channel := cached.channelByGroupID[*req.GroupID]; channel != nil && channel.RestrictModels {
			return ctx, nil, false
		}
	}
	var thresholds map[string]int
	if limits := s.rateLimitService; limits != nil && limits.settingService != nil {
		cached, _ := accountSchedulingThresholdsCache.Load().(*cachedAccountSchedulingThresholds)
		if cached == nil || !cached.complete || len(cached.thresholds) == 0 || now.UnixNano() >= cached.expiresAt {
			return ctx, nil, false
		}
		thresholds = cached.thresholds
	}
	return ctx, thresholds, true
}
