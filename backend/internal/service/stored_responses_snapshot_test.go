//go:build unit

package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStoredResponsesSchedulerUsesOwningRowsWithRedactedSnapshot(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(14500)
			full := storedResponsesTestAccount(14501, groupID)
			metadata := full
			metadata.Credentials = map[string]any{"api_key": full.Credentials["api_key"], "base_url": full.Credentials["base_url"], "openai_capabilities": full.Credentials["openai_capabilities"]}
			repo := schedulerTestOpenAIAccountRepo{accounts: []Account{full}}
			cache := &snapshotHydrationCache{snapshot: []*Account{&metadata}, accounts: map[int64]*Account{full.ID: &full}}
			svc := &OpenAIGatewayService{accountRepo: repo, cfg: &config.Config{}, schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil), rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			selection, _, err := svc.SelectAccountWithSchedulerForCapability(context.Background(), &groupID, "", "", "fixture-text", nil, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityBPSLocalResponses, false, false, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, full.ID, selection.Account.ID)
			require.True(t, selection.Acquired)
			_, err = BPSStoredResponsesConfigForAccount(selection.Account)
			require.NoError(t, err)
			selection.ReleaseFunc()
			require.NotContains(t, metadata.Credentials, "bps_responses_service_key")
			// Ordinary selection still consumes the existing redacted candidate cache.
			ordinary, err := svc.listSchedulableAccounts(context.Background(), &groupID, PlatformOpenAI)
			require.NoError(t, err)
			require.Len(t, ordinary, 1)
			require.NotContains(t, ordinary[0].Credentials, "bps_responses_service_key")
		})
	}
}
