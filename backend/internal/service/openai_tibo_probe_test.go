package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestTiboProbeRequiresExplicitEnablement(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{}
	require.False(t, svc.openAITiboRouteApplies(account))
	svc.cfg = &config.Config{}
	require.False(t, svc.openAITiboRouteApplies(account))
	on := true
	svc.cfg.Gateway.OpenAITiboRoute.Enabled = &on
	require.True(t, svc.openAITiboRouteApplies(account))
	svc.DisableOpenAITiboRouteForTest()
	require.False(t, svc.openAITiboRouteApplies(account))
}

func TestTiboProbeEligibilityExcludesIndependentBackends(t *testing.T) {
	for _, tt := range []struct {
		name        string
		kind        string
		platform    string
		extra       map[string]any
		credentials map[string]any
		want        bool
	}{
		{name: "OAuth", kind: AccountTypeOAuth, want: true},
		{name: "setup token", kind: AccountTypeSetupToken, want: true},
		{name: "API key", kind: AccountTypeAPIKey, want: true},
		{name: "CPR", kind: AccountTypeCPR},
		{name: "other platform", kind: AccountTypeOAuth, platform: PlatformAnthropic},
		{name: "gateway pool", kind: AccountTypeOAuth, extra: map[string]any{openAIGatewayPoolExtraKey: true}},
		{name: "BPS declaration", kind: AccountTypeAPIKey, credentials: map[string]any{"bps_files_contract": "bps-files/1"}},
		{name: "partial BPS declaration", kind: AccountTypeAPIKey, credentials: map[string]any{"bps_files_control_url": "https://bps.example.invalid"}},
		{name: "BPS capability", kind: AccountTypeAPIKey, credentials: map[string]any{openAIEndpointCapabilitiesCredentialKey: []string{"responses", "bps_files"}}},
		{name: "Agent Identity", kind: AccountTypeOAuth, credentials: map[string]any{openAIAuthModeCredentialKey: OpenAIAuthModeAgentIdentity}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			platform := tt.platform
			if platform == "" {
				platform = PlatformOpenAI
			}
			account := &Account{Platform: platform, Type: tt.kind, Extra: tt.extra, Credentials: tt.credentials}
			require.Equal(t, tt.want, IsOpenAITiboAccount(account))
		})
	}
	require.False(t, IsOpenAITiboAccount(nil))
}

func TestTiboHTTPObservationRequiresSuccessfulUnambiguousCompletion(t *testing.T) {
	completion := string(cookieWSCompletion(openAICodexTicketDefaultModel, "True"))
	for _, tt := range []struct {
		name, body string
		verdict    openAITiboVerdict
	}{
		{"True", "data: " + completion + "\n\n", openAITiboHealthy},
		{"False", "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "False")) + "\n\n", openAITiboDegraded},
		{"different model", "data: " + string(cookieWSCompletion("gpt-6-luna", "True")) + "\n\n", openAITiboDegraded},
		{"empty", "", openAITiboUnknown},
		{"only done marker", "data: [DONE]\n\n", openAITiboUnknown},
		{"failed terminal", "data: {\"type\":\"response.failed\"}\n\n", openAITiboUnknown},
		{"failed before completion", "data: {\"type\":\"response.failed\"}\n\ndata: " + completion + "\n\n", openAITiboUnknown},
		{"error after completion", "data: " + completion + "\n\ndata: {\"type\":\"error\"}\n\n", openAITiboUnknown},
		{"duplicate completion", "data: " + completion + "\n\ndata: " + completion + "\n\n", openAITiboUnknown},
		{"incomplete status", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\",\"model\":\"gpt-6-astra\"}}\n\n", openAITiboUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := openAITiboSampleFromObservation(http.StatusOK, observeOpenAITiboHTTPProbe([]byte(tt.body)))
			require.Equal(t, tt.verdict, got.verdict)
		})
	}
}

func TestTiboAPIProbeUsesSelectedCredentialsAndBoundedFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			response := cookieWSHTTPResponse("True")
			response.StatusCode = status
			tc := newTiboRouteCase(t, false, false, response)
			tc.account.Type = AccountTypeAPIKey
			tc.account.Credentials = map[string]any{"api_key": "selected-key", "base_url": "https://selected.example.invalid/v1"}
			got := tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account)
			require.Len(t, tc.upstream.requests, 1)
			req := tc.upstream.requests[0]
			require.Equal(t, "https://selected.example.invalid/v1/responses", req.URL.String())
			require.Equal(t, "Bearer selected-key", req.Header.Get("Authorization"))
			require.Contains(t, string(tc.upstream.bodies[0]), openAITiboProbePrompt)
			if status == http.StatusOK {
				require.Equal(t, openAITiboHealthy, got.verdict)
			} else {
				require.Equal(t, openAITiboUnknown, got.verdict)
			}
		})
	}
}

func TestTiboProbeBodyLimitRejectsSuccessfulPrefix(t *testing.T) {
	body := "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "True")) + "\n\n"
	body += strings.Repeat(" ", openAITiboProbeBodyLimit)
	require.Equal(t, openAITiboUnknown, openAITiboSampleFromObservation(http.StatusOK, observeOpenAITiboHTTPProbe([]byte(body))).verdict)
}
