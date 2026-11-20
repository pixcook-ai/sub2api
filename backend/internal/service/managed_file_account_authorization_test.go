package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type managedAccountAuthorizationGroups struct {
	GroupRepository
	group *Group
	err   error
	reads int
}

func (r *managedAccountAuthorizationGroups) GetByIDLite(context.Context, int64) (*Group, error) {
	r.reads++
	return r.group, r.err
}

func TestManagedFileAccountAuthorizationRejectsChangedEffectiveMapping(t *testing.T) {
	groupID := int64(91001)
	selected := managedSchedulerTestAccount(91002, groupID)
	selected.Credentials["model_mapping"] = map[string]any{"gpt-public": "gpt-old"}
	fresh := managedSchedulerTestAccount(selected.ID, groupID)
	fresh.Credentials["model_mapping"] = map[string]any{"gpt-public": "gpt-new"}
	group := &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{fresh}}, cfg: &config.Config{}, schedulerSnapshot: &SchedulerSnapshotService{groupRepo: &managedAccountAuthorizationGroups{group: group}}}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	_, err := svc.AuthorizeManagedFileAccount(ctx, groupID, "gpt-public", &selected)
	require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked, "a fresh account that still owns the alias cannot authorize an old wire-model mapping")
}

type managedAccountAuthorizationRoutes struct {
	CompositeModelRouteRepository
	routes []CompositeModelRoute
	err    error
	reads  int
}

func (r *managedAccountAuthorizationRoutes) ListByGroup(_ context.Context, _ int64, _ bool) ([]CompositeModelRoute, error) {
	r.reads++
	return append([]CompositeModelRoute(nil), r.routes...), r.err
}

type managedAccountAuthorizationAccounts struct {
	AccountRepository
	current      *Account
	catalog      []Account
	catalogErr   error
	catalogReads int
}

func (r *managedAccountAuthorizationAccounts) GetByID(context.Context, int64) (*Account, error) {
	return r.current, nil
}
func (r *managedAccountAuthorizationAccounts) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	r.catalogReads++
	return r.catalog, r.catalogErr
}

func managedAccountAuthorizationFixture(composite bool) (*OpenAIGatewayService, context.Context, *Account, *managedAccountAuthorizationGroups, *managedAccountAuthorizationAccounts, *managedAccountAuthorizationRoutes) {
	const groupID int64 = 91001
	selected := managedSchedulerTestAccount(91002, groupID)
	current := managedSchedulerTestAccount(selected.ID, groupID)
	group := &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
	groups := &managedAccountAuthorizationGroups{group: group}
	accounts := &managedAccountAuthorizationAccounts{current: &current}
	routes := &managedAccountAuthorizationRoutes{}
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true})
	if composite {
		group.Platform = PlatformComposite
		route := CompositeModelRoute{ID: 21, GroupID: groupID, PublicModel: "gpt-public", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-route", Endpoint: CompositeRouteEndpointResponses, Enabled: true}
		routes.routes = []CompositeModelRoute{route}
		ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceExplicit, GroupID: groupID, PublicModel: "gpt-public", TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-route", Endpoint: CompositeRouteEndpointResponses})
		ctx = WithManagedFileRouteResolver(ctx, NewCompositeRouteResolver(routes))
	}
	return &OpenAIGatewayService{accountRepo: accounts, cfg: &config.Config{}, schedulerSnapshot: &SchedulerSnapshotService{groupRepo: groups}}, ctx, &selected, groups, accounts, routes
}

func TestManagedFileAccountAuthorizationRechecksCurrentCompositeRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*managedAccountAuthorizationRoutes)
		want   error
	}{
		{"unchanged", func(*managedAccountAuthorizationRoutes) {}, nil},
		{"upstream changed", func(r *managedAccountAuthorizationRoutes) { r.routes[0].UpstreamModel = "gpt-other" }, ErrBPSRequestAuthorizationRevoked},
		{"target changed", func(r *managedAccountAuthorizationRoutes) { r.routes[0].TargetPlatform = PlatformAnthropic }, ErrBPSRequestAuthorizationRevoked},
		{"route removed", func(r *managedAccountAuthorizationRoutes) { r.routes = nil }, ErrBPSRequestAuthorizationRevoked},
		{"source unavailable", func(r *managedAccountAuthorizationRoutes) { r.err = errors.New("private routes URL") }, ErrBPSRequestAuthorizationUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, selected, groups, _, routes := managedAccountAuthorizationFixture(true)
			tc.change(routes)
			current, err := svc.AuthorizeManagedFileAccount(ctx, groups.group.ID, "gpt-public", selected)
			if tc.want == nil {
				require.NoError(t, err)
				require.Equal(t, selected.ID, current.ID)
			} else {
				require.ErrorIs(t, err, tc.want)
				require.Nil(t, current)
				require.NotContains(t, err.Error(), "private")
			}
			require.Equal(t, 1, groups.reads)
			require.Equal(t, 1, routes.reads)
		})
	}
}

func TestManagedFileAccountAuthorizationRequiresFreshGroupPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*managedAccountAuthorizationGroups, *Account)
		want   error
	}{
		{"unchanged", func(*managedAccountAuthorizationGroups, *Account) {}, nil},
		{"disabled group", func(g *managedAccountAuthorizationGroups, _ *Account) { g.group.Status = StatusDisabled }, ErrBPSRequestAuthorizationRevoked},
		{"wrong group", func(g *managedAccountAuthorizationGroups, _ *Account) { g.group.ID++ }, ErrBPSRequestAuthorizationRevoked},
		{"model revoked", func(g *managedAccountAuthorizationGroups, _ *Account) {
			g.group.ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: []string{"different-model"}}
		}, ErrBPSRequestAuthorizationRevoked},
		{"platform drift", func(g *managedAccountAuthorizationGroups, _ *Account) { g.group.Platform = PlatformAnthropic }, ErrBPSRequestAuthorizationRevoked},
		{"new privacy requirement", func(g *managedAccountAuthorizationGroups, _ *Account) { g.group.RequirePrivacySet = true }, ErrBPSRequestAuthorizationRevoked},
		{"new privacy requirement satisfied", func(g *managedAccountAuthorizationGroups, a *Account) {
			g.group.RequirePrivacySet = true
			a.Extra["privacy_mode"] = PrivacyModeTrainingOff
		}, nil},
		{"group removed", func(g *managedAccountAuthorizationGroups, _ *Account) { g.err = ErrGroupNotFound }, ErrBPSRequestAuthorizationRevoked},
		{"group database unavailable", func(g *managedAccountAuthorizationGroups, _ *Account) { g.err = errors.New("private group database") }, ErrBPSRequestAuthorizationUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, selected, groups, accounts, _ := managedAccountAuthorizationFixture(false)
			tc.change(groups, accounts.current)
			current, err := svc.AuthorizeManagedFileAccount(ctx, 91001, "gpt-public", selected)
			if tc.want == nil {
				require.NoError(t, err)
				require.NotNil(t, current)
			} else {
				require.ErrorIs(t, err, tc.want)
				require.Nil(t, current)
			}
			require.Equal(t, 1, groups.reads, "the old context Group is not the authorization source")
		})
	}
}

func TestManagedFileAccountAuthorizationPinsEffectiveAccountAndBackend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"route model mapping changed", func(a *Account) { a.Credentials["model_mapping"] = map[string]any{"gpt-route": "gpt-new"} }},
		{"public model mapping changed", func(a *Account) { a.Credentials["model_mapping"] = map[string]any{"gpt-public": "gpt-new"} }},
		{"API key rotated", func(a *Account) { a.Credentials["api_key"] = "private-new" }},
		{"base URL changed", func(a *Account) { a.Credentials["base_url"] = "https://private.invalid" }},
		{"identity changed", func(a *Account) { a.Credentials["bps_files_credential_generation"] = "next" }},
		{"account moved group", func(a *Account) { a.GroupIDs = []int64{999} }},
		{"account disabled", func(a *Account) { a.Status = StatusDisabled }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, selected, _, accounts, _ := managedAccountAuthorizationFixture(true)
			tc.change(accounts.current)
			current, err := svc.AuthorizeManagedFileAccount(ctx, 91001, "gpt-public", selected)
			require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
			require.Nil(t, current)
			require.NotContains(t, err.Error(), "private")
		})
	}
}

func TestManagedFileAccountAuthorizationUncachedOwnershipCannotFallBackAfterCatalogError(t *testing.T) {
	svc, ctx, selected, _, accounts, routes := managedAccountAuthorizationFixture(true)
	routes.routes = nil
	accounts.catalogErr = errors.New("private catalog unavailable")
	ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceDetector, GroupID: 91001, PublicModel: "gpt-public", TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-public", Endpoint: CompositeRouteEndpointResponses})
	_, err := svc.AuthorizeManagedFileAccount(ctx, 91001, "gpt-public", selected)
	require.ErrorIs(t, err, ErrBPSRequestAuthorizationUnavailable, "a recognizable model cannot turn an unavailable fresh catalog into guessed proof")
	require.Equal(t, 1, accounts.catalogReads)
}

func TestManagedFileAccountAuthorizationUncachedOwnershipRejectsAmbiguity(t *testing.T) {
	svc, ctx, selected, _, accounts, routes := managedAccountAuthorizationFixture(true)
	routes.routes = nil
	one := managedSchedulerTestAccount(91002, 91001)
	one.Credentials["model_mapping"] = map[string]any{"gpt-public": "gpt-native"}
	two := one
	two.ID++
	two.Platform = PlatformAnthropic
	accounts.current = &one
	accounts.catalog = []Account{one, two}
	ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceAccount, GroupID: 91001, PublicModel: "gpt-public", TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-public", Endpoint: CompositeRouteEndpointResponses})
	_, err := svc.AuthorizeManagedFileAccount(ctx, 91001, "gpt-public", selected)
	require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
	require.Equal(t, 1, accounts.catalogReads)
}

func TestManagedFileAccountAuthorizationMissingPortsAndCancellationFailClosed(t *testing.T) {
	svc, ctx, selected, groups, _, _ := managedAccountAuthorizationFixture(true)
	ctx = WithManagedFileRouteResolver(ctx, nil)
	_, err := svc.AuthorizeManagedFileAccount(ctx, 91001, "gpt-public", selected)
	require.ErrorIs(t, err, ErrBPSRequestAuthorizationUnavailable)
	svc.schedulerSnapshot = nil
	_, err = svc.AuthorizeManagedFileAccount(context.Background(), 91001, "gpt-public", selected)
	require.ErrorIs(t, err, ErrBPSRequestAuthorizationUnavailable)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	groups.reads = 0
	_, err = svc.AuthorizeManagedFileAccount(cancelled, 91001, "gpt-public", selected)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, groups.reads)
}
