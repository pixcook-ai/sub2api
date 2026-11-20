package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type tiboQualityAdmissionRepo struct {
	AccountRepository
	accounts map[int64]*Account
	err      error
}

func (r *tiboQualityAdmissionRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts[id], nil
}

func TestTiboQualityIsolationPreservesManualSchedulingState(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_tibo_quality_isolated": true}}
	require.False(t, account.IsSchedulable())
	require.False(t, account.IsCredentialUsableForShadow())
	require.True(t, account.Schedulable, "quality isolation must not overwrite the administrator switch")
	account.Schedulable = false
	account.Extra["openai_tibo_quality_isolated"] = false
	require.False(t, account.IsSchedulable(), "recovery cannot resume a manual pause")
	account.Schedulable = true
	require.True(t, account.IsSchedulable())
}

func TestTiboQualityIsolationForwardEntriesRejectStaleAccount(t *testing.T) {
	stale := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	fresh := *stale
	fresh.Extra = map[string]any{"openai_tibo_quality_isolated": true}
	svc := &OpenAIGatewayService{accountRepo: &tiboQualityAdmissionRepo{accounts: map[int64]*Account{1: &fresh}}}
	body := []byte("{}")
	for name, forward := range map[string]func() (*OpenAIForwardResult, error){
		"responses": func() (*OpenAIForwardResult, error) { return svc.Forward(context.Background(), nil, stale, body) },
		"chat": func() (*OpenAIForwardResult, error) {
			return svc.ForwardAsChatCompletions(context.Background(), nil, stale, body, "", "")
		},
		"messages": func() (*OpenAIForwardResult, error) {
			return svc.ForwardAsAnthropic(context.Background(), nil, stale, body, "", "")
		},
		"images": func() (*OpenAIForwardResult, error) {
			return svc.ForwardImages(context.Background(), nil, stale, body, nil, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := forward()
			require.Nil(t, result)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, http.StatusServiceUnavailable, failover.ClientStatusCode)
			require.Equal(t, GatewayFailureReason("openai_tibo_quality_isolated"), failover.Reason)
			require.True(t, failover.ShouldRetryNextAccount())
		})
	}
}

func TestTiboQualityIsolationContinuationStopsAccountFailover(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_tibo_quality_isolated": true}}
	svc := &OpenAIGatewayService{}
	result, err := svc.Forward(context.Background(), nil, account, []byte("{\"previous_response_id\":\"resp_bound\"}"))
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.ShouldRetryNextAccount())
}

func TestTiboQualityIsolationRejectsShadowOfIsolatedParent(t *testing.T) {
	parentID := int64(1)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_tibo_quality_isolated": true}}
	shadow := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ParentAccountID: &parentID}
	svc := &OpenAIGatewayService{accountRepo: &tiboQualityAdmissionRepo{accounts: map[int64]*Account{1: parent, 2: shadow}}}
	result, err := svc.Forward(context.Background(), nil, shadow, []byte("{}"))
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
}

func TestTiboQualityIsolationCurrentRowReadFailureDoesNotSend(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	svc := &OpenAIGatewayService{accountRepo: &tiboQualityAdmissionRepo{err: errors.New("database unavailable")}}
	result, err := svc.Forward(context.Background(), nil, account, []byte("{}"))
	require.Nil(t, result)
	require.ErrorContains(t, err, "verify OpenAI quality isolation")
}

func TestTiboQualityIsolationSchedulerSkipsIsolatedStickyForBothModes(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "legacy"
		if advanced {
			name = "advanced"
		}
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(98101)
			accounts := []Account{
				{ID: 98111, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Concurrency: 1, GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_tibo_quality_isolated": true}},
				{ID: 98112, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Concurrency: 1, GroupIDs: []int64{groupID}, Priority: 10},
			}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.LBTopK = 1
			svc := &OpenAIGatewayService{
				accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: cfg,
				cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:quality-session": 98111}},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "false")
			}
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "quality-session", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(98112), selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestTiboQualityIsolationBoundResponseDoesNotFallBackOrForgetOwner(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "legacy"
		if advanced {
			name = "advanced"
		}
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			ctx := context.Background()
			groupID := int64(98201)
			accounts := []Account{
				{ID: 98211, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
					GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_tibo_quality_isolated": true}},
				{ID: 98212, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}},
			}
			cfg := newSchedulerTestOpenAIWSV2Config()
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: cfg,
				cache: &schedulerTestGatewayCache{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "false")
			}
			store := svc.getOpenAIWSStateStore()
			require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_quality_bound", 98211, time.Hour))
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "resp_quality_bound", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selection)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.False(t, failover.ShouldRetryNextAccount())
			owner, err := store.GetResponseAccount(ctx, groupID, "resp_quality_bound")
			require.NoError(t, err)
			require.Equal(t, int64(98211), owner)
		})
	}
}
