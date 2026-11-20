package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestManagedForwardAuthorizesOriginalPublicModelBeforeDescribe(t *testing.T) {
	for _, composite := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel", true: "composite"}[composite], func(t *testing.T) {
			svc, ctx, selected, groups, _, _ := managedAccountAuthorizationFixture(composite)
			upstream := &httpUpstreamRecorder{}
			svc.httpUpstream = upstream
			groups.group.ID = 2
			selected.GroupIDs = []int64{2}
			// Request authorization must reject before any native Describe, even
			// though the body was already rewritten by server routing.
			ctx = context.WithValue(ctx, ctxkey.Model, "gpt-public")
			if composite {
				ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceExplicit, GroupID: 2, PublicModel: "gpt-public", TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-route", Endpoint: CompositeRouteEndpointResponses})
			}
			seen := ""
			ctx = WithBPSRequestReauthorize(ctx, func(_ context.Context, model string) error {
				seen = model
				return ErrBPSRequestAuthorizationRevoked
			})
			files := managedResolverFiles(t, &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}})
			resolver := NewManagedFileResolver(files, &managedResolverLedger{}, upstream, svc.cfg)
			body := []byte(`{"model":"gpt-route","store":false,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`)
			plan, err := resolver.Prepare(ctx, FileOwner{UserID: 1, APIKeyID: 3, GroupID: 2}, body)
			require.NoError(t, err)
			defer plan.Close()
			svc.managedFiles = resolver
			ctx = WithManagedFilePlan(ctx, plan)
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			_, err = svc.Forward(ctx, c, selected, body)
			require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
			require.Equal(t, "gpt-public", seen)
			require.Empty(t, upstream.requests)
			var local *ManagedFilePreparationError
			require.True(t, errors.As(err, &local))
		})
	}
}

func TestManagedForwardRechecksFreshAccountBeforeDescribe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*managedAccountAuthorizationGroups, *managedAccountAuthorizationAccounts)
		revoked bool
	}{
		{"unchanged", func(*managedAccountAuthorizationGroups, *managedAccountAuthorizationAccounts) {}, false},
		{"group disabled", func(g *managedAccountAuthorizationGroups, _ *managedAccountAuthorizationAccounts) {
			g.group.Status = StatusDisabled
		}, true},
		{"public mapping changed", func(_ *managedAccountAuthorizationGroups, a *managedAccountAuthorizationAccounts) {
			a.current.Credentials["model_mapping"] = map[string]any{"gpt-public": "gpt-other", "gpt-route": "gpt-route"}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, selected, groups, accounts, _ := managedAccountAuthorizationFixture(false)
			upstream := &httpUpstreamRecorder{err: errors.New("fixture stops after Describe")}
			svc.httpUpstream = upstream
			ctx = context.WithValue(ctx, ctxkey.Model, "gpt-public")
			ctx = WithBPSRequestReauthorize(ctx, func(context.Context, string) error { return nil })
			file := managedResolverRecord("file-a")
			file.GroupID = groups.group.ID
			files := managedResolverFiles(t, &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": file}})
			resolver := NewManagedFileResolver(files, &managedResolverLedger{}, upstream, svc.cfg)
			body := []byte(`{"model":"gpt-route","store":false,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`)
			plan, err := resolver.Prepare(ctx, FileOwner{UserID: 1, APIKeyID: 3, GroupID: groups.group.ID}, body)
			require.NoError(t, err)
			defer plan.Close()
			svc.managedFiles = resolver
			ctx = WithManagedFilePlan(ctx, plan)
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			tc.change(groups, accounts)
			_, err = svc.Forward(ctx, c, selected, body)
			require.Error(t, err)
			if tc.revoked {
				require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
				require.Empty(t, upstream.requests)
			} else {
				require.Len(t, upstream.requests, 1)
				require.Contains(t, upstream.lastReq.URL.Path, "describe")
			}
			require.Equal(t, 1, groups.reads)
		})
	}
}
