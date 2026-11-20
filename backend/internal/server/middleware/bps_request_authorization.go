package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Only immutable authenticated identity and the ingress security IP survive
// into deferred authorization. A callback never reads a reusable Gin context.
type requestAuthorizationFacts struct {
	keyID, userID, groupID int64
	clientIP               string
}

func captureRequestAuthorizationFacts(key *service.APIKey, clientIP string) requestAuthorizationFacts {
	facts := requestAuthorizationFacts{keyID: key.ID, userID: key.UserID, clientIP: clientIP}
	if key.GroupID != nil {
		facts.groupID = *key.GroupID
	}
	return facts
}

func isBPSRequestAuthorizationPath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	path = strings.TrimRight(path, "/")
	for _, root := range []string{"/v1/responses", "/openai/v1/responses", "/responses", "/backend-api/codex/responses"} {
		if path == root || path == root+"/compact" {
			return true
		}
	}
	return false
}

// Reuse publication's cache-bypassing identity, group, IP and subscription
// checks. Files retain their unbilled quota semantics and OpenAI-only group.
func freshRequestAuthorization(ctx context.Context, keys *service.APIKeyService, subscriptions *service.SubscriptionService, credential string, facts requestAuthorizationFacts, openAIOnly bool) (*service.APIKey, *service.UserSubscription, error) {
	fresh, err := keys.GetForFilePublication(ctx, credential)
	if err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			return nil, nil, service.ErrFileAuthorizationRevoked
		}
		return nil, nil, service.ErrFileAuthorizationUnavailable
	}
	if fresh == nil || fresh.ID != facts.keyID || fresh.UserID != facts.userID || fresh.GroupID == nil || facts.groupID <= 0 || *fresh.GroupID != facts.groupID || fresh.User == nil || !fresh.User.IsActive() || (fresh.Status != service.StatusActive && fresh.Status != service.StatusAPIKeyQuotaExhausted) || fresh.IsExpired() {
		return nil, nil, service.ErrFileAuthorizationRevoked
	}
	if _, _, ok := validateAPIKeyGroupAvailable(fresh); !ok {
		return nil, nil, service.ErrFileAuthorizationRevoked
	}
	if !validateAPIKeyGroupAllowed(fresh) || fresh.Group == nil || openAIOnly && fresh.Group.Platform != service.PlatformOpenAI {
		return nil, nil, service.ErrFileAuthorizationRevoked
	}
	if len(fresh.IPWhitelist) > 0 || len(fresh.IPBlacklist) > 0 {
		allowed, _ := ip.CheckIPRestrictionWithCompiledRules(facts.clientIP, fresh.CompiledIPWhitelist, fresh.CompiledIPBlacklist)
		if !allowed {
			return nil, nil, service.ErrFileAuthorizationRevoked
		}
	}
	var sub *service.UserSubscription
	if fresh.Group.IsSubscriptionType() {
		if subscriptions == nil {
			return nil, nil, service.ErrFileAuthorizationUnavailable
		}
		sub, err = subscriptions.GetForFilePublication(ctx, fresh.UserID, *fresh.GroupID)
		if err != nil && !errors.Is(err, service.ErrSubscriptionNotFound) {
			return nil, nil, service.ErrFileAuthorizationUnavailable
		}
		if err != nil || sub == nil || !sub.IsActive() {
			return nil, nil, service.ErrFileAuthorizationRevoked
		}
	}
	return fresh, sub, nil
}

func newBPSRequestReauthorize(keys *service.APIKeyService, subscriptions *service.SubscriptionService, cfg *config.Config, credential string, facts requestAuthorizationFacts) service.BPSRequestReauthorize {
	return func(ctx context.Context, requestedModel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fresh, sub, err := freshRequestAuthorization(ctx, keys, subscriptions, credential, facts, false)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, service.ErrFileAuthorizationRevoked) {
				return service.ErrBPSRequestAuthorizationRevoked
			}
			return service.ErrBPSRequestAuthorizationUnavailable
		}
		if fresh.User.ID != facts.userID || fresh.User.DeletedAt != nil || fresh.Group.ID != facts.groupID || fresh.Status != service.StatusActive || fresh.IsQuotaExhausted() || requestedModel == "" || strings.TrimSpace(requestedModel) != requestedModel || !fresh.Group.ModelAllowlist.Allows(requestedModel) {
			return service.ErrBPSRequestAuthorizationRevoked
		}
		// A cached OpenAI route cannot authorize a group changed to a native
		// platform. Composite's resolved OpenAI target is fenced by the caller.
		if fresh.Group.Platform != service.PlatformOpenAI && fresh.Group.Platform != service.PlatformComposite {
			return service.ErrBPSRequestAuthorizationRevoked
		}
		if fresh.RateLimit5h > 0 && fresh.EffectiveUsage5h() >= fresh.RateLimit5h || fresh.RateLimit1d > 0 && fresh.EffectiveUsage1d() >= fresh.RateLimit1d || fresh.RateLimit7d > 0 && fresh.EffectiveUsage7d() >= fresh.RateLimit7d {
			return service.ErrBPSRequestAuthorizationRevoked
		}
		if sub != nil {
			if sub.UserID != facts.userID || sub.GroupID != facts.groupID || sub.DeletedAt != nil {
				return service.ErrBPSRequestAuthorizationRevoked
			}
			maintenance, validationErr := subscriptions.ValidateAndCheckLimits(sub, fresh.Group)
			if maintenance {
				sub, err = subscriptions.EnsureWindowMaintenance(ctx, sub)
				if err != nil {
					return service.ErrBPSRequestAuthorizationUnavailable
				}
				if sub == nil || !sub.IsActive() || sub.UserID != facts.userID || sub.GroupID != facts.groupID || sub.DeletedAt != nil {
					return service.ErrBPSRequestAuthorizationRevoked
				}
				_, validationErr = subscriptions.ValidateAndCheckLimits(sub, fresh.Group)
			}
			if validationErr != nil {
				return service.ErrBPSRequestAuthorizationRevoked
			}
		} else if apiKeyBalanceBelowAuthThreshold(fresh.User.Balance, cfg) {
			return service.ErrBPSRequestAuthorizationRevoked
		}
		return ctx.Err()
	}
}
