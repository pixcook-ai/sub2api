//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestManagedFileSchedulerFirstUseWithCredentialRedactedSnapshot(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(87001)
			full := managedSchedulerTestAccount(87002, groupID)
			metadata := full
			metadata.Credentials = map[string]any{
				"api_key":             full.Credentials["api_key"],
				"base_url":            full.Credentials["base_url"],
				"openai_capabilities": full.Credentials["openai_capabilities"],
			}
			repo := schedulerTestOpenAIAccountRepo{accounts: []Account{full}}
			snapshotCache := &snapshotHydrationCache{
				snapshot: []*Account{&metadata},
				accounts: map[int64]*Account{full.ID: &full},
			}
			svc := &OpenAIGatewayService{
				accountRepo: repo, cfg: &config.Config{},
				schedulerSnapshot:  NewSchedulerSnapshotService(snapshotCache, nil, nil, nil, nil),
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "", "gpt-5.5", 0)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, full.ID, selection.Account.ID)
			require.True(t, selection.Acquired)
			require.Nil(t, selection.WaitPlan)
			_, err = BPSFilesConfigForAccount(selection.Account)
			require.NoError(t, err)
			require.NotContains(t, metadata.Credentials, "bps_files_service_key")
			selection.ReleaseFunc()
		})
	}
}
