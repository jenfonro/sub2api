package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This fake enforces the public per-account concurrency contract rather than
// preselecting the scheduler's expected winner.
type stickyOverflowConcurrencyCache struct {
	schedulerTestConcurrencyCache
	counts         map[int64]int
	limits         map[int64]int
	loadBatchCalls int
	staleLoadOnce  bool
}

func (c *stickyOverflowConcurrencyCache) AcquireAccountSlot(ctx context.Context, id int64, limit int, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.limits[id] = limit
	if c.counts[id] >= limit {
		return false, nil
	}
	c.counts[id]++
	return true, nil
}

func (c *stickyOverflowConcurrencyCache) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	c.counts[id]--
	return nil
}

func (c *stickyOverflowConcurrencyCache) GetAccountsLoadBatch(_ context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	c.loadBatchCalls++
	if c.loadBatchErr != nil {
		return nil, c.loadBatchErr
	}
	loads := make(map[int64]*AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		loads[account.ID] = &AccountLoadInfo{AccountID: account.ID, CurrentConcurrency: c.counts[account.ID], LoadRate: c.counts[account.ID] * 100 / account.MaxConcurrency}
		if snapshot, ok := c.loadMap[account.ID]; ok {
			loads[account.ID] = snapshot
		}
	}
	if c.staleLoadOnce {
		c.loadMap = nil
	}
	return loads, nil
}

type stickyOverflowMode struct {
	name     string
	advanced bool
	batch    bool
	loadErr  bool
}

var stickyOverflowModes = []stickyOverflowMode{
	{name: "legacy_batch", batch: true},
	{name: "legacy_without_batch"},
	{name: "legacy_batch_failure", batch: true, loadErr: true},
	{name: "advanced", advanced: true, batch: true},
}

func newStickyOverflowTestService(t *testing.T, mode stickyOverflowMode, enabled bool) (*OpenAIGatewayService, *stickyOverflowConcurrencyCache, *schedulerTestGatewayCache) {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	settings := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
		SettingKeyOpenAIStickySessionOverflowEnabled: strconv.FormatBool(enabled),
		openAIAdvancedSchedulerSettingKey:            strconv.FormatBool(mode.advanced),
	}}
	rate := 1.0
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 0, GroupIDs: []int64{1}, RateMultiplier: &rate},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 1, GroupIDs: []int64{1}, RateMultiplier: &rate},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:overflow": 1}}
	concurrency := &stickyOverflowConcurrencyCache{counts: map[int64]int{1: 10}, limits: make(map[int64]int)}
	if mode.loadErr {
		concurrency.loadBatchErr = errors.New("load snapshot unavailable")
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	// Isolate the capacity policy from the independently configurable legacy
	// health-escape policy. Zero thresholds mean "use defaults", not disabled.
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = false
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.Scheduling.LoadBatchEnabled = mode.batch
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 120 * time.Second
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 100
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 30 * time.Second
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   &RateLimitService{settingService: &SettingService{settingRepo: settings}},
		concurrencyService: NewConcurrencyService(concurrency),
	}
	return svc, concurrency, cache
}

func selectStickyOverflow(svc *OpenAIGatewayService, ctx context.Context, previous string) (*AccountSelectionResult, error) {
	groupID := int64(1)
	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, previous, "overflow", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	return selection, err
}

func TestOpenAIStickySessionOverflow_MigratesAndDoesNotReturnWhenOriginalRecovers(t *testing.T) {
	for _, mode := range stickyOverflowModes {
		t.Run(mode.name, func(t *testing.T) {
			svc, slots, cache := newStickyOverflowTestService(t, mode, true)
			selection, err := selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.True(t, selection.Acquired)
			require.Nil(t, selection.WaitPlan, "an idle B must be used without queueing on A")
			require.Equal(t, int64(2), selection.Account.ID)
			require.Equal(t, int64(2), cache.sessionBindings["openai:overflow"])
			require.Equal(t, 10, slots.counts[1])
			require.Equal(t, 1, slots.counts[2])
			require.Equal(t, 10, slots.limits[1])
			require.Equal(t, 10, slots.limits[2], "account limits are independent and unchanged")
			selection.ReleaseFunc()
			require.Zero(t, slots.counts[2])
			if !mode.batch {
				require.Zero(t, slots.loadBatchCalls, "disabled batch load must stay disabled")
			}

			slots.counts[1] = 0
			selection, err = selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.True(t, selection.Acquired)
			require.Equal(t, int64(2), selection.Account.ID, "A recovering must not cause a return migration")
			selection.ReleaseFunc()

			repo := svc.accountRepo.(schedulerTestOpenAIAccountRepo)
			repo.accounts[1].Schedulable = false
			selection, err = selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.True(t, selection.Acquired)
			require.Equal(t, int64(1), selection.Account.ID, "an unavailable B must still use existing failover")
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])
			selection.ReleaseFunc()
		})
	}
}

func TestOpenAIStickySessionOverflow_DisabledKeepsStickyWait(t *testing.T) {
	for _, mode := range stickyOverflowModes {
		t.Run(mode.name, func(t *testing.T) {
			svc, slots, cache := newStickyOverflowTestService(t, mode, false)
			selection, err := selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.False(t, selection.Acquired)
			require.Equal(t, int64(1), selection.Account.ID)
			require.Equal(t, 120*time.Second, selection.WaitPlan.Timeout)
			require.Equal(t, 3, selection.WaitPlan.MaxWaiting)
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])
			require.Zero(t, slots.counts[2])
		})
	}
}

func TestOpenAIStickySessionOverflow_AllFullOrCanceledNeverRebinds(t *testing.T) {
	for _, mode := range stickyOverflowModes {
		t.Run(mode.name, func(t *testing.T) {
			svc, slots, cache := newStickyOverflowTestService(t, mode, true)
			slots.counts[2] = 10
			selection, err := selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.False(t, selection.Acquired)
			require.NotNil(t, selection.WaitPlan)
			require.Equal(t, 10, selection.WaitPlan.MaxConcurrency)
			require.Equal(t, 20, slots.counts[1]+slots.counts[2])
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])

			// A stale load snapshot can advertise B as idle after another request
			// has taken its final slot. The atomic acquisition remains authoritative.
			slots.loadMap = map[int64]*AccountLoadInfo{2: {AccountID: 2, LoadRate: 0}}
			svc.concurrencyService = NewConcurrencyService(slots)
			selection, err = selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.False(t, selection.Acquired)
			require.NotNil(t, selection.WaitPlan)
			require.Equal(t, 20, slots.counts[1]+slots.counts[2])
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])

			slots.counts[2] = 0
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			selection, _ = selectStickyOverflow(svc, ctx, "")
			if selection != nil {
				require.False(t, selection.Acquired)
			}
			require.Zero(t, slots.counts[2])
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])
		})
	}
}

func TestOpenAIStickySessionOverflow_PreservesStrictResponseAndGuardian(t *testing.T) {
	for _, mode := range stickyOverflowModes {
		t.Run(mode.name, func(t *testing.T) {
			svc, slots, cache := newStickyOverflowTestService(t, mode, true)
			require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(context.Background(), 1, "resp_owner", 1, time.Hour))
			selection, err := selectStickyOverflow(svc, context.Background(), "resp_owner")
			require.NoError(t, err)
			require.False(t, selection.Acquired)
			require.Equal(t, int64(1), selection.Account.ID)
			require.NotNil(t, selection.WaitPlan)

			ctx := context.WithValue(context.Background(), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "overflow"})
			selection, err = selectStickyOverflow(svc, ctx, "")
			require.NoError(t, err)
			require.False(t, selection.Acquired)
			require.Equal(t, int64(1), selection.Account.ID)
			require.NotNil(t, selection.WaitPlan)
			require.Zero(t, slots.counts[2])
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])
		})
	}
}

func TestOpenAIStickySessionOverflow_ProfitGateRebindsOnlyAdmittedCapacityMigration(t *testing.T) {
	for _, mode := range stickyOverflowModes {
		for _, profitVeto := range []bool{false, true} {
			t.Run(mode.name+"/profit_veto="+strconv.FormatBool(profitVeto), func(t *testing.T) {
				svc, _, cache := newStickyOverflowTestService(t, mode, true)
				if profitVeto {
					rate := 3.0
					svc.accountRepo.(schedulerTestOpenAIAccountRepo).accounts[0].RateMultiplier = &rate
				}
				gateCtx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 1, platform: PlatformOpenAI, threshold: 2})
				selection, err := selectStickyOverflow(svc, gateCtx, "")
				require.NoError(t, err)
				require.True(t, selection.Acquired)
				require.Equal(t, int64(2), selection.Account.ID)
				require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"], "selection alone must not bypass admission")
				admissionCtx := ContextWithSelectionProfitGate(context.Background(), selection)
				_, vetoed, _ := svc.ProfitControlVetoLatest(admissionCtx, selection.Account)
				require.False(t, vetoed)
				groupID := int64(1)
				require.NoError(t, svc.BindStickySessionAfterProfitAdmission(admissionCtx, &groupID, "overflow", 2))
				want := int64(2)
				if profitVeto {
					want = 1
				}
				require.Equal(t, want, cache.sessionBindings["openai:overflow"], "profit-only spillover must remain temporary")
				selection.ReleaseFunc()
			})
		}
	}
}

type stickyOverflowRuntimeSettingRepo struct {
	openAIAdvancedSchedulerSettingRepoStub
	batchErr   error
	batchReads int
	valueReads int
}

func (r *stickyOverflowRuntimeSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	r.batchReads++
	if r.batchErr != nil {
		return nil, r.batchErr
	}
	return r.openAIAdvancedSchedulerSettingRepoStub.GetMultiple(ctx, keys)
}

func (r *stickyOverflowRuntimeSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.valueReads++
	return r.openAIAdvancedSchedulerSettingRepoStub.GetValue(ctx, key)
}

func TestOpenAIStickySessionOverflow_RuntimeSettingsDefaultAndCachedFallback(t *testing.T) {
	for _, batchFailure := range []bool{false, true} {
		for _, value := range []string{"", "false", "true"} {
			t.Run("batch_failure="+strconv.FormatBool(batchFailure)+"/value="+value, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
				repo := &stickyOverflowRuntimeSettingRepo{
					openAIAdvancedSchedulerSettingRepoStub: openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{}},
				}
				if value != "" {
					repo.values[SettingKeyOpenAIStickySessionOverflowEnabled] = value
				}
				if batchFailure {
					repo.batchErr = errors.New("settings batch unavailable")
				}
				svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: &SettingService{settingRepo: repo}}}
				require.Equal(t, value == "true", svc.isOpenAIStickySessionOverflowEnabled(context.Background()))
				reads := repo.valueReads
				require.Equal(t, value == "true", svc.isOpenAIStickySessionOverflowEnabled(context.Background()))
				require.Equal(t, 1, repo.batchReads, "runtime settings must be cached")
				require.Equal(t, reads, repo.valueReads)
			})
		}
	}
}

func TestOpenAIStickySessionOverflow_RechecksCachedFullPoolBeforeWaiting(t *testing.T) {
	svc, slots, cache := newStickyOverflowTestService(t, stickyOverflowMode{batch: true}, true)
	slots.loadMap = map[int64]*AccountLoadInfo{2: {AccountID: 2, CurrentConcurrency: 10, LoadRate: 100}}
	slots.staleLoadOnce = true

	selection, err := selectStickyOverflow(svc, context.Background(), "")

	require.NoError(t, err)
	require.True(t, selection.Acquired, "stale full-load metadata must not hide the actually available B slot")
	require.Equal(t, int64(2), selection.Account.ID)
	require.Equal(t, int64(2), cache.sessionBindings["openai:overflow"])
	selection.ReleaseFunc()
}

func TestOpenAIStickySessionOverflow_DoesNotReusePriorAdmissionForProfitSpillover(t *testing.T) {
	for _, mode := range stickyOverflowModes {
		t.Run(mode.name, func(t *testing.T) {
			svc, _, cache := newStickyOverflowTestService(t, mode, true)
			gateCtx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 1, platform: PlatformOpenAI, threshold: 2})
			first, err := selectStickyOverflow(svc, gateCtx, "")
			require.NoError(t, err)
			require.True(t, first.Acquired)
			admissionCtx := ContextWithSelectionProfitGate(context.Background(), first)
			first.ReleaseFunc()
			// No terminal binding occurred for the earlier attempt. A now fails
			// profit admission, so the next selection's reason is no longer capacity.
			rate := 3.0
			svc.accountRepo.(schedulerTestOpenAIAccountRepo).accounts[0].RateMultiplier = &rate
			next, err := selectStickyOverflow(svc, admissionCtx, "")
			require.NoError(t, err)
			require.True(t, next.Acquired)
			require.Equal(t, int64(2), next.Account.ID)
			groupID := int64(1)
			require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ContextWithSelectionProfitGate(admissionCtx, next), &groupID, "overflow", next.Account.ID))
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"])
			next.ReleaseFunc()
		})
	}
}

func TestOpenAIStickySessionOverflow_PreservesExistingHealthEscape(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			svc, _, cache := newStickyOverflowTestService(t, stickyOverflowMode{advanced: true, batch: true}, enabled)
			svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			selection, err := selectStickyOverflow(svc, context.Background(), "")
			require.NoError(t, err)
			require.True(t, selection.Acquired)
			require.Equal(t, int64(2), selection.Account.ID)
			wantBinding := int64(1) // the existing health escape is temporary
			if enabled {
				wantBinding = 2 // the new capacity policy explicitly migrates
			}
			require.Equal(t, wantBinding, cache.sessionBindings["openai:overflow"])
			selection.ReleaseFunc()
		})
	}
	for _, mode := range stickyOverflowModes {
		t.Run("guardian/"+mode.name, func(t *testing.T) {
			svc, _, cache := newStickyOverflowTestService(t, mode, true)
			svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			ctx := context.WithValue(context.Background(), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "overflow"})
			selection, err := selectStickyOverflow(svc, ctx, "")
			require.NoError(t, err)
			require.Equal(t, int64(1), cache.sessionBindings["openai:overflow"], "existing guardian health escape must never migrate the parent binding")
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}
