//go:build unit

package service

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type stickyOverflowSettingRepoStub struct {
	settingUpdateRepoStub
	values    map[string]string
	readCalls int
}

func (s *stickyOverflowSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	s.readCalls++
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (s *stickyOverflowSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	s.readCalls++
	values := make(map[string]string, len(s.values))
	for key, value := range s.values {
		values[key] = value
	}
	return values, nil
}

func (s *stickyOverflowSettingRepoStub) SetMultiple(ctx context.Context, values map[string]string) error {
	if err := s.settingUpdateRepoStub.SetMultiple(ctx, values); err != nil {
		return err
	}
	if s.values == nil {
		s.values = make(map[string]string)
	}
	for key, value := range values {
		s.values[key] = value
	}
	return nil
}

func TestSettingService_GetAllSettings_OpenAIStickySessionOverflow(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values map[string]string
		want   bool
	}{
		{name: "missing defaults to disabled"},
		{name: "empty defaults to disabled", values: map[string]string{SettingKeyOpenAIStickySessionOverflowEnabled: ""}},
		{name: "disabled", values: map[string]string{SettingKeyOpenAIStickySessionOverflowEnabled: "false"}},
		{name: "enabled", values: map[string]string{SettingKeyOpenAIStickySessionOverflowEnabled: "true"}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewSettingService(&settingGetAllRepoStub{values: tt.values}, &config.Config{})

			settings, err := svc.GetAllSettings(context.Background())

			require.NoError(t, err)
			require.Equal(t, tt.want, settings.OpenAIStickySessionOverflowEnabled)
		})
	}
}

func TestSettingService_InitializeDefaultSettings_OpenAIStickySessionOverflowDisabled(t *testing.T) {
	repo := &stickyOverflowSettingRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	require.NoError(t, svc.InitializeDefaultSettings(context.Background()))
	require.Equal(t, "false", repo.values[SettingKeyOpenAIStickySessionOverflowEnabled])

	settings, err := svc.GetAllSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.OpenAIStickySessionOverflowEnabled)
}

func TestSettingService_UpdateSettings_OpenAIStickySessionOverflowRoundTripAndCache(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	repo := &stickyOverflowSettingRepoStub{}
	svc := NewSettingService(repo, &config.Config{})
	gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: svc}}
	ctx := context.Background()

	for _, enabled := range []bool{false, true, false} {
		require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
			OpenAIStickySessionOverflowEnabled: enabled,
		}))
		require.Equal(t, strconv.FormatBool(enabled), repo.values[SettingKeyOpenAIStickySessionOverflowEnabled])

		settings, err := svc.GetAllSettings(ctx)
		require.NoError(t, err)
		require.Equal(t, enabled, settings.OpenAIStickySessionOverflowEnabled)
		readsBefore := repo.readCalls
		require.Equal(t, enabled, gateway.isOpenAIStickySessionOverflowEnabled(ctx))
		require.Equal(t, enabled, gateway.isOpenAIStickySessionOverflowEnabled(ctx))
		require.Equal(t, readsBefore, repo.readCalls, "saved settings must be served from cache")
	}
}

func TestSettingService_UpdateSettingsOmitting_OpenAIStickySessionOverflowKeepsStoredValueAndCache(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			repo := &stickyOverflowSettingRepoStub{}
			svc := NewSettingService(repo, &config.Config{})
			gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: svc}}
			ctx := context.Background()
			require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
				OpenAIStickySessionOverflowEnabled: enabled,
			}))

			require.NoError(t, svc.UpdateSettingsOmitting(ctx, &SystemSettings{
				SiteName:                           "Updated Gateway",
				OpenAIStickySessionOverflowEnabled: !enabled,
			}, OmittedSettingKeys{SettingKeyOpenAIStickySessionOverflowEnabled: {}}))

			require.NotContains(t, repo.updates, SettingKeyOpenAIStickySessionOverflowEnabled)
			require.Equal(t, "Updated Gateway", repo.values[SettingKeySiteName])
			require.Equal(t, strconv.FormatBool(enabled), repo.values[SettingKeyOpenAIStickySessionOverflowEnabled])
			settings, err := svc.GetAllSettings(ctx)
			require.NoError(t, err)
			require.Equal(t, enabled, settings.OpenAIStickySessionOverflowEnabled)
			require.Equal(t, enabled, gateway.isOpenAIStickySessionOverflowEnabled(ctx))
		})
	}
}

func TestSettingService_UpdateSettings_OpenAIStickySessionOverflowWriteFailureKeepsCache(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	repo := &stickyOverflowSettingRepoStub{}
	svc := NewSettingService(repo, &config.Config{})
	gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: svc}}
	ctx := context.Background()
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{
		OpenAIStickySessionOverflowEnabled: true,
	}))

	repo.setMultipleErr = errors.New("settings write failed")
	err := svc.UpdateSettings(ctx, &SystemSettings{OpenAIStickySessionOverflowEnabled: false})

	require.ErrorIs(t, err, repo.setMultipleErr)
	require.Equal(t, "true", repo.values[SettingKeyOpenAIStickySessionOverflowEnabled])
	settings, err := svc.GetAllSettings(ctx)
	require.NoError(t, err)
	require.True(t, settings.OpenAIStickySessionOverflowEnabled)
	require.True(t, gateway.isOpenAIStickySessionOverflowEnabled(ctx))
}
