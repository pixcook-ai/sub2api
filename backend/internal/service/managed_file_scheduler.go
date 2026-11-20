package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
)

const OpenAIEndpointCapabilityBPSFiles OpenAIEndpointCapability = "bps_files"

type managedFileRequiredAccountKey struct{}
type managedFileCandidatePassKey struct{}

// BPSFilesConfigForAccount accepts only an explicit, native Responses backend.
// A general OpenAI-compatible URL or an omitted capability is never sufficient.
func BPSFilesConfigForAccount(a *Account) (BPSFilesBackendConfig, error) {
	var c BPSFilesBackendConfig
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeAPIKey ||
		a.UsesGatewayPool() || a.getExtraBool(openAIQualityGuardEnabledKey) || a.UsesOpenAIRawRelay() ||
		a.routesByModel() || a.RoutesProtocolByInbound() || a.IsOpenAIPassthroughEnabled() ||
		a.IsOpenAIResponsesWebSocketV2Enabled() || openai_compat.ResolveResponsesSupport(a.Extra) != openai_compat.ResponsesSupportYes {
		return c, fmt.Errorf("managed files: unsupported account route")
	}
	capabilities, _ := a.openAIEndpointCapabilitySet()
	if !capabilities[string(OpenAIEndpointCapabilityBPSFiles)] ||
		!a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) ||
		a.GetCredential("bps_files_contract") != "bps-files/1" ||
		strings.TrimSpace(a.GetCredential("base_url")) == "" || strings.TrimSpace(a.GetCredential("api_key")) == "" {
		return c, fmt.Errorf("managed files: missing explicit backend contract")
	}
	c = BPSFilesBackendConfig{
		ControlURL: a.GetCredential("bps_files_control_url"), ServiceKey: a.GetCredential("bps_files_service_key"),
		Expected: managedfiles.Identity{
			BackendID:            a.GetCredential("bps_files_backend_id"),
			SubjectFingerprint:   a.GetCredential("bps_files_subject_fingerprint"),
			CredentialGeneration: a.GetCredential("bps_files_credential_generation"),
		},
		AccountID: a.ID, Concurrency: a.Concurrency,
	}
	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(c.ControlURL) != c.ControlURL ||
		!bpsFilesOpaque(c.ServiceKey, 4096) || managedfiles.ValidateIdentity(c.Expected) != nil || c.AccountID <= 0 || c.Concurrency <= 0 {
		return BPSFilesBackendConfig{}, fmt.Errorf("managed files: invalid backend configuration")
	}
	if a.Proxy != nil {
		c.ProxyURL = a.Proxy.URL()
	}
	return c, nil
}

// SelectAccountWithSchedulerForManagedFiles consumes a server-resolved account
// constraint. Callers must never derive requiredAccountID from public headers.
// File operations intentionally do not read or update inference sticky sessions.
func (s *OpenAIGatewayService) SelectAccountWithSchedulerForManagedFiles(ctx context.Context, groupID *int64, sessionHash, requestedModel string, requiredAccountID int64) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	if requiredAccountID < 0 {
		return nil, OpenAIAccountScheduleDecision{}, ErrNoAvailableAccounts
	}
	ctx = context.WithValue(ctx, managedFileRequiredAccountKey{}, requiredAccountID)
	return s.selectAccountWithScheduler(ctx, groupID, "", "", requestedModel, nil, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityBPSFiles, "", false, PlatformOpenAI, false, false)
}

// RecheckManagedFileAccount always reads the owning row, including when the
// scheduler snapshot is disabled or the application runs in SimpleMode.
func (s *OpenAIGatewayService) RecheckManagedFileAccount(ctx context.Context, groupID *int64, accountID int64, model string) (*Account, error) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	return s.recheckManagedFileAccount(ctx, groupID, accountID, model, nil)
}

func (s *OpenAIGatewayService) recheckManagedFileAccount(ctx context.Context, groupID *int64, accountID int64, model string, excluded map[int64]struct{}) (*Account, error) {
	ctx = context.WithValue(ctx, openAIProxyStreamQuarantineBypassKey{}, false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.accountRepo == nil || accountID <= 0 || s.checkChannelPricingRestriction(ctx, groupID, model) {
		return nil, ErrNoAvailableAccounts
	}
	if _, blocked := excluded[accountID]; blocked {
		return nil, ErrNoAvailableAccounts
	}
	if parent := s.resolveOpenAIGuardianParentAccountID(ctx, groupID); parent > 0 && parent != accountID {
		return nil, ErrNoAvailableAccounts
	}
	a, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || a == nil || a.ID != accountID || !openAIStickyAccountMatchesGroup(a, groupID) ||
		(a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)) {
		return nil, ErrNoAvailableAccounts
	}
	if !isOpenAICompatibleAccountEligibleForRequestBeforeProfit(ctx, a, PlatformOpenAI, model, false, OpenAIEndpointCapabilityBPSFiles) ||
		s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return nil, ErrNoAvailableAccounts
	}
	checker := &defaultOpenAIAccountScheduler{service: s}
	if ok, _ := checker.isAccountRequestCompatibleReason(ctx, a, OpenAIAccountScheduleRequest{
		GroupID: groupID, Platform: PlatformOpenAI, RequestedModel: model,
		RequiredTransport: OpenAIUpstreamTransportHTTPSSE, RequiredCapability: OpenAIEndpointCapabilityBPSFiles,
		RequirePrivacySet: s.openAIGroupRequiresPrivacySet(ctx, groupID),
	}); !ok {
		return nil, ErrNoAvailableAccounts
	}
	return a, nil
}

func (s *OpenAIGatewayService) selectManagedFileAccount(ctx context.Context, groupID *int64, model string, excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: openAIAccountScheduleLayerLoadBalance}
	if err := ctx.Err(); err != nil {
		return nil, decision, err
	}
	ctx = context.WithValue(ctx, openAIProxyStreamQuarantineBypassKey{}, false)
	requiredID, _ := ctx.Value(managedFileRequiredAccountKey{}).(int64)
	if requiredID == 0 {
		// Reuse the ordinary engine's ordering and load awareness while the BPS
		// capability filters out all pool, relay and implicit-compatible rows.
		selection, next, err := s.selectAccountWithSchedulerOnce(context.WithValue(ctx, managedFileCandidatePassKey{}, true), groupID, "", "", model, excluded, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityBPSFiles, "", false, PlatformOpenAI, false, false)
		if err != nil {
			return nil, next, err
		}
		if selection == nil || selection.Account == nil {
			return nil, next, ErrNoAvailableAccounts
		}
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		// Candidate filtering for the first use is handled below; do not ever
		// convert the file operation into an account wait plan.
		if !selection.Acquired || selection.WaitPlan != nil {
			return nil, next, ErrNoAvailableAccounts
		}
		requiredID = selection.Account.ID
	}
	a, err := s.recheckManagedFileAccount(ctx, groupID, requiredID, model, excluded)
	if err != nil {
		return nil, decision, err
	}
	before, err := BPSFilesConfigForAccount(a)
	if err != nil {
		return nil, decision, ErrNoAvailableAccounts
	}
	apiKey, baseURL := a.GetCredential("api_key"), a.GetCredential("base_url")
	acquired, err := s.tryAcquireAccountSlot(ctx, a.ID, a.Concurrency)
	if err != nil {
		return nil, decision, err
	}
	if acquired == nil || !acquired.Acquired {
		return nil, decision, ErrNoAvailableAccounts
	}
	latest, err := s.recheckManagedFileAccount(ctx, groupID, requiredID, model, excluded)
	var after BPSFilesBackendConfig
	if err == nil {
		after, err = BPSFilesConfigForAccount(latest)
	}
	if err != nil || before != after || latest.GetCredential("api_key") != apiKey || latest.GetCredential("base_url") != baseURL {
		if acquired.ReleaseFunc != nil {
			acquired.ReleaseFunc()
		}
		return nil, decision, ErrNoAvailableAccounts
	}
	decision.SelectedAccountID, decision.SelectedAccountType = latest.ID, latest.Type
	return attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: latest, Acquired: true, ReleaseFunc: acquired.ReleaseFunc}), decision, nil
}
