package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OpenAIOAuthRefreshSuccessRepository is a mandatory persistence boundary for
// ordinary managed OpenAI OAuth refresh. The complete credential document and
// selected proxy used by the upstream attempt must still match. A successful
// update and scheduler outbox publication must commit atomically.
type OpenAIOAuthRefreshSuccessRepository interface {
	UpdateOpenAIOAuthCredentialsIfUnchanged(context.Context, int64, map[string]any, *int64, map[string]any) (bool, error)
}

func isStoredOpenAIManagedOAuth(account *Account) bool {
	return account != nil && account.IsOpenAIOAuth() &&
		!account.IsOpenAIPersonalAccessToken() && !account.IsOpenAIAgentIdentity() &&
		!strings.HasPrefix(strings.TrimSpace(account.GetOpenAIAccessToken()), "at-")
}

type openAIManualRefreshContextKey struct{}

func openAIRefreshPersistenceUnavailable(cause error) error {
	return &providerCycleContainmentRefreshError{err: &oauthRefreshStateUnavailableError{err: cause}}
}

func openAIRefreshCASNotConfigured() error {
	return &providerConfigurationRefreshError{err: &oauthRefreshStateUnavailableError{
		err: errors.New("OpenAI OAuth refresh success CAS repository is not configured"),
	}}
}

func (api *OAuthRefreshAPI) persistOpenAIRefreshSuccess(ctx context.Context, attempted *Account, credentials map[string]any, executor OAuthRefreshExecutor, cacheKey string) (*OAuthRefreshResult, error) {
	repository, ok := api.accountRepo.(OpenAIOAuthRefreshSuccessRepository)
	if !ok {
		return nil, openAIRefreshCASNotConfigured()
	}
	applied, err := repository.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, attempted.ID, attempted.Credentials, attempted.ProxyID, credentials)
	if err != nil {
		return nil, openAIRefreshPersistenceUnavailable(fmt.Errorf("%w: %v", errOAuthRefreshCredentialPersist, err))
	}
	// The SQL update (and its outbox) has succeeded. In a surrounding admin
	// transaction, all non-transactional publications wait until its commit.
	if applied {
		AfterAccountCommit(ctx, func(committed context.Context) {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(committed), defaultRefreshPostPersistCleanupTimeout)
			defer stop()
			if api.tokenCache != nil {
				_ = api.tokenCache.DeleteAccessToken(cleanup, cacheKey)
			}
			if publisher, ok := executor.(interface {
				AuthCredentialsPersisted(context.Context, *Account)
			}); ok {
				publisher.AuthCredentialsPersisted(cleanup, attempted)
			}
		})
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRefreshPostPersistCleanupTimeout)
	defer cancel()
	current, readErr := api.accountRepo.GetByID(cleanupCtx, attempted.ID)
	if readErr != nil || current == nil || current.ID != attempted.ID {
		return nil, openAIRefreshPersistenceUnavailable(errors.New("OpenAI OAuth refresh persistence result is unavailable"))
	}
	if !applied {
		// Preserve the winner without publishing or caching the stale response.
		// Request callers independently check owner continuity before replay;
		// background callers treat Refreshed=false as an already-refreshed skip.
		return &OAuthRefreshResult{Account: current}, nil
	}

	// An admin may have replaced credentials immediately after our CAS. Keep
	// the current snapshot and avoid reporting our obsolete map as its refresh.
	expectedJSON, expectedErr := json.Marshal(credentials)
	currentJSON, currentErr := json.Marshal(current.Credentials)
	if expectedErr != nil || currentErr != nil || string(expectedJSON) != string(currentJSON) {
		return &OAuthRefreshResult{Account: current}, nil
	}
	return &OAuthRefreshResult{Refreshed: true, NewCredentials: credentials, Account: current}, nil
}

func (s *OpenAIOAuthService) SetStoredAccountRefresh(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	s.storedRefreshAPI = api
	s.storedRefreshExecutor = executor
}

type openAIManualRefreshExecutor struct {
	OAuthRefreshExecutor
	expected *Account
}

func (executor *openAIManualRefreshExecutor) NeedsRefresh(account *Account, _ time.Duration) bool {
	return openAIRefreshFingerprint(account) == openAIRefreshFingerprint(executor.expected)
}

func (executor *openAIManualRefreshExecutor) AuthCredentialsPersisted(ctx context.Context, previous *Account) {
	if publisher, ok := executor.OAuthRefreshExecutor.(interface {
		AuthCredentialsPersisted(context.Context, *Account)
	}); ok {
		publisher.AuthCredentialsPersisted(ctx, previous)
	}
}

// RefreshStoredAccount is the admin entry for ordinary managed OAuth. It uses
// exactly the same coordinator and row-scoped locks as request/background
// refresh. PAT validation and other auth modes keep their separate paths.
func (s *OpenAIOAuthService) RefreshStoredAccount(ctx context.Context, expected *Account) (*Account, error) {
	if s == nil || s.storedRefreshAPI == nil || s.storedRefreshExecutor == nil {
		return nil, openAIRefreshCASNotConfigured()
	}
	if !isStoredOpenAIManagedOAuth(expected) {
		return nil, errors.New("OpenAI OAuth account is not refreshable")
	}
	ctx = context.WithValue(ctx, openAIManualRefreshContextKey{}, true)
	executor := &openAIManualRefreshExecutor{OAuthRefreshExecutor: s.storedRefreshExecutor, expected: snapshotOAuthRefreshAccount(expected)}
	result, err := s.storedRefreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), expected, executor, 0)
	if err != nil {
		return nil, err
	}
	if result == nil || result.LockHeld || result.Account == nil {
		return nil, errors.New("OpenAI OAuth refresh is already in progress")
	}
	if !sameOpenAIOAuthOwner(expected, result.Account) {
		return nil, errOAuthRefreshAccountStateChanged
	}
	return result.Account, nil
}
