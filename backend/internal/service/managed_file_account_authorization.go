package service

import (
	"context"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type managedFileRouteResolverKey struct{}

// WithManagedFileRouteResolver binds a server-owned current-route source.
func WithManagedFileRouteResolver(ctx context.Context, resolver *CompositeRouteResolver) context.Context {
	return context.WithValue(ctx, managedFileRouteResolverKey{}, resolver)
}

// AuthorizeManagedFileAccount is called before native work and generation.
// requestedModel is the original client model, before route/account rewriting.
func (s *OpenAIGatewayService) AuthorizeManagedFileAccount(ctx context.Context, groupID int64, requestedModel string, selected *Account) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if selected == nil || groupID <= 0 || requestedModel == "" || strings.TrimSpace(requestedModel) != requestedModel {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	if s == nil || s.schedulerSnapshot == nil || s.schedulerSnapshot.groupRepo == nil {
		return nil, ErrBPSRequestAuthorizationUnavailable
	}
	group, err := s.schedulerSnapshot.GetGroupByIDLite(ctx, groupID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrGroupNotFound) {
			return nil, ErrBPSRequestAuthorizationRevoked
		}
		return nil, ErrBPSRequestAuthorizationUnavailable
	}
	if group == nil || group.ID != groupID || !IsGroupContextValid(group) || !group.IsActive() || !group.ModelAllowlist.Allows(requestedModel) {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	forwardModel := requestedModel
	switch group.Platform {
	case PlatformComposite:
		var routeErr error
		forwardModel, routeErr = s.managedCurrentCompositeModel(ctx, groupID, requestedModel)
		if routeErr != nil {
			return nil, routeErr
		}
	case PlatformOpenAI:
		if _, resolved := ResolvedTargetPlatformFromContext(ctx); resolved {
			return nil, ErrBPSRequestAuthorizationRevoked
		}
	default:
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	// Replace stale group policy and previously computed profit gates before
	// reusing the ordinary account eligibility path against the owning row.
	freshCtx := context.WithValue(ctx, ctxkey.Group, group)
	freshCtx = context.WithValue(freshCtx, openAIProfitControlGateCtxKey{}, (*openAIProfitControlGate)(nil))
	current, err := s.RecheckManagedFileAccount(freshCtx, &groupID, selected.ID, forwardModel)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	before, err := BPSFilesConfigForAccount(selected)
	if err != nil {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	after, err := BPSFilesConfigForAccount(current)
	if err != nil || before != after || current.GetCredential("api_key") != selected.GetCredential("api_key") || current.GetCredential("base_url") != selected.GetCredential("base_url") {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	for _, model := range []string{requestedModel, forwardModel} {
		for _, compact := range []bool{false, true} {
			oldBilling, oldUpstream := resolveOpenAIForwardMappedModels(selected, model, compact)
			newBilling, newUpstream := resolveOpenAIForwardMappedModels(current, model, compact)
			if oldBilling != newBilling || oldUpstream != newUpstream {
				return nil, ErrBPSRequestAuthorizationRevoked
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *OpenAIGatewayService) managedCurrentCompositeModel(ctx context.Context, groupID int64, requestedModel string) (string, error) {
	resolver, _ := ctx.Value(managedFileRouteResolverKey{}).(*CompositeRouteResolver)
	if resolver == nil || resolver.repo == nil || s.accountRepo == nil {
		return "", ErrBPSRequestAuthorizationUnavailable
	}
	target, targetOK := ResolvedTargetPlatformFromContext(ctx)
	upstream, upstreamOK := ResolvedUpstreamModelFromContext(ctx)
	public, publicOK := RequestedPublicModelFromContext(ctx)
	source, sourceOK := CompositeRouteSourceFromContext(ctx)
	if !targetOK || target != PlatformOpenAI || !upstreamOK || !publicOK || public != requestedModel || !sourceOK {
		return "", ErrBPSRequestAuthorizationRevoked
	}
	// The ingress resolver may use the model catalog cache. A fresh resolver
	// uses the same ownership fold with no cache and refuses its error fallback.
	fresh := NewCompositeRouteResolver(resolver.repo)
	ownership := &GatewayService{accountRepo: s.accountRepo}
	var ownershipErr error
	fresh.SetModelOwnershipResolver(func(c context.Context, id int64, model string) (CompositeModelOwnership, error) {
		result, err := ownership.resolveCompositeModelOwnership(c, id, model)
		ownershipErr = err
		return result, err
	})
	decision, err := fresh.Resolve(ctx, groupID, requestedModel, CompositeRouteEndpointResponses)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || ownershipErr != nil {
		return "", ErrBPSRequestAuthorizationUnavailable
	}
	if !decision.Matched || decision.GroupID != groupID || decision.PublicModel != requestedModel || decision.TargetPlatform != target || decision.UpstreamModel != upstream || decision.Source != source || decision.Endpoint != CompositeRouteEndpointResponses {
		return "", ErrBPSRequestAuthorizationRevoked
	}
	return decision.UpstreamModel, nil
}
