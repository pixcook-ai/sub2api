package service

import (
	"context"
	"time"
)

// BPSStoredResponseRoute fixes storage semantics for the complete request. Only
// server configuration determines Local; client store never selects a subpool.
type BPSStoredResponseRoute struct{ Local bool }

func (s *OpenAIGatewayService) ResolveBPSStoredResponseRoute(ctx context.Context, groupID *int64, requestedModel, platform string) (BPSStoredResponseRoute, error) {
	var route BPSStoredResponseRoute
	if s == nil || s.accountRepo == nil {
		return route, ErrBPSStoredResponseRoute
	}
	platform = NormalizeOpenAICompatiblePlatform(platform)
	if platform != PlatformOpenAI {
		return route, nil
	}
	if err := ctx.Err(); err != nil {
		return route, err
	}
	accounts, err := s.listOpenAIModelAvailabilityCandidates(ctx, groupID, platform)
	if err != nil {
		return route, ErrBPSStoredResponseRoute
	}
	var ordinary, local bool
	for i := range accounts {
		a := &accounts[i]
		if a.Platform != platform || a.Status != StatusActive || !a.Schedulable || !s.openAIAccountMatchesSchedulingGroup(a, groupID) {
			continue
		}
		if requestedModel != "" && !supportsOpenAIRequestModel(a, requestedModel, false) {
			continue
		}
		if source, ok := CompositeRouteSourceFromContext(ctx); ok && source == CompositeRouteSourceAccount {
			if publicModel, ok := RequestedPublicModelFromContext(ctx); ok && !explicitModelMappingClaims(*a, publicModel) {
				continue
			}
		}
		if declaresBPSStoredResponses(a) {
			if _, err := BPSStoredResponsesConfigForAccount(a); err != nil {
				return route, ErrBPSStoredResponseRoute
			}
			local = true
		} else {
			ordinary = true
		}
		if ordinary && local {
			return route, ErrBPSStoredResponseRoute
		}
	}
	if !ordinary && !local {
		return route, ErrNoAvailableAccounts
	}
	route.Local = local
	return route, nil
}

// RecheckBPSStoredResponseAccount bypasses scheduler snapshots for the local
// Responses route. The caller also binds the complete configuration before send.
func (s *OpenAIGatewayService) RecheckBPSStoredResponseAccount(ctx context.Context, groupID *int64, accountID int64, model string) (*Account, error) {
	return s.recheckBPSStoredResponseAccount(ctx, groupID, accountID, model, true)
}

func (s *OpenAIGatewayService) recheckBPSStoredResponseAccount(ctx context.Context, groupID *int64, accountID int64, model string, local bool) (*Account, error) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return nil, ErrBPSStoredResponseRoute
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	ctx = context.WithValue(ctx, openAIProxyStreamQuarantineBypassKey{}, false)
	if s.checkChannelPricingRestriction(ctx, groupID, model) {
		return nil, ErrBPSStoredResponseRoute
	}
	if parent := s.resolveOpenAIGuardianParentAccountID(ctx, groupID); parent > 0 && parent != accountID {
		return nil, ErrBPSStoredResponseRoute
	}
	a, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || a == nil || a.ID != accountID || !s.openAIAccountMatchesSchedulingGroup(a, groupID) ||
		(local && a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)) || declaresBPSStoredResponses(a) != local {
		return nil, ErrBPSStoredResponseRoute
	}
	capability := OpenAIEndpointCapability("")
	if local {
		capability = OpenAIEndpointCapabilityBPSLocalResponses
	}
	if !isOpenAICompatibleAccountEligibleForRequestBeforeProfit(ctx, a, PlatformOpenAI, model, false, capability) || s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return nil, ErrBPSStoredResponseRoute
	}
	checker := &defaultOpenAIAccountScheduler{service: s}
	if ok, _ := checker.isAccountRequestCompatibleReason(ctx, a, OpenAIAccountScheduleRequest{
		GroupID: groupID, Platform: PlatformOpenAI, RequestedModel: model,
		RequiredTransport: OpenAIUpstreamTransportAny, RequiredCapability: capability,
		RequirePrivacySet: s.openAIGroupRequiresPrivacySet(ctx, groupID),
	}); !ok {
		return nil, ErrBPSStoredResponseRoute
	}
	return a, nil
}

// RecheckBPSStoredResponseRoute keeps the request in its original storage domain
// across slot acquisition. It also fences all private credentials for local
// routes, so a rotated row must be selected by a new request.
func (s *OpenAIGatewayService) RecheckBPSStoredResponseRoute(ctx context.Context, groupID *int64, selected *Account, model string, route BPSStoredResponseRoute) (*Account, error) {
	if selected == nil || declaresBPSStoredResponses(selected) != route.Local {
		return nil, ErrBPSStoredResponseRoute
	}
	latest, err := s.recheckBPSStoredResponseAccount(ctx, groupID, selected.ID, model, route.Local)
	if err != nil {
		return nil, err
	}
	if route.Local {
		before, beforeErr := BPSStoredResponsesConfigForAccount(selected)
		after, afterErr := BPSStoredResponsesConfigForAccount(latest)
		if beforeErr != nil || afterErr != nil || before != after || selected.GetCredential("api_key") != latest.GetCredential("api_key") || selected.GetCredential("base_url") != latest.GetCredential("base_url") {
			return nil, ErrBPSStoredResponseRoute
		}
	}
	return latest, nil
}
