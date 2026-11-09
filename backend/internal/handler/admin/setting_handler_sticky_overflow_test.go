//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingHandler_GetSettings_OpenAIStickySessionOverflowDefaultsToDisabled(t *testing.T) {
	handler, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)

	handler.GetSettings(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body.Data, service.SettingKeyOpenAIStickySessionOverflowEnabled)
	require.Equal(t, false, body.Data[service.SettingKeyOpenAIStickySessionOverflowEnabled])
}

func TestSettingHandler_UpdateSettings_OpenAIStickySessionOverflowRoundTripAndAudit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		initial string
		payload map[string]any
		want    bool
		changed bool
	}{
		{
			name:    "absent defaults to disabled",
			payload: map[string]any{"site_name": "Updated Gateway"},
		},
		{
			name:    "enable",
			payload: map[string]any{"openai_sticky_session_overflow_enabled": true},
			want:    true,
			changed: true,
		},
		{
			name:    "omitted keeps enabled",
			initial: "true",
			payload: map[string]any{"site_name": "Updated Gateway"},
			want:    true,
		},
		{
			name:    "omitted keeps disabled",
			initial: "false",
			payload: map[string]any{"site_name": "Updated Gateway"},
		},
		{
			name:    "explicit disable",
			initial: "true",
			payload: map[string]any{"openai_sticky_session_overflow_enabled": false},
			changed: true,
		},
		{
			name:    "unchanged enabled",
			initial: "true",
			payload: map[string]any{"openai_sticky_session_overflow_enabled": true},
			want:    true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stored := map[string]string{}
			if tt.initial != "" {
				stored[service.SettingKeyOpenAIStickySessionOverflowEnabled] = tt.initial
			}
			handler, repo := newStepUpSwitchTestHandler(t, stored)
			var auditLog bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&auditLog, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })

			updateRec := doUpdateSettings(t, handler, tt.payload, nil)
			require.Equal(t, http.StatusOK, updateRec.Code, updateRec.Body.String())
			require.Equal(t, strconv.FormatBool(tt.want), repo.values[service.SettingKeyOpenAIStickySessionOverflowEnabled])

			getRec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(getRec)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
			handler.GetSettings(c)
			require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
			for _, rec := range []*httptest.ResponseRecorder{updateRec, getRec} {
				var body struct {
					Data map[string]any `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				require.Contains(t, body.Data, service.SettingKeyOpenAIStickySessionOverflowEnabled)
				require.Equal(t, tt.want, body.Data[service.SettingKeyOpenAIStickySessionOverflowEnabled])
			}

			var changed []string
			for _, line := range bytes.Split(bytes.TrimSpace(auditLog.Bytes()), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var entry struct {
					Message string   `json:"msg"`
					Audit   bool     `json:"audit"`
					Changed []string `json:"changed"`
				}
				require.NoError(t, json.Unmarshal(line, &entry))
				if entry.Message == "settings updated" && entry.Audit {
					changed = append(changed, entry.Changed...)
				}
			}
			if tt.changed {
				require.Contains(t, changed, service.SettingKeyOpenAIStickySessionOverflowEnabled)
			} else {
				require.NotContains(t, changed, service.SettingKeyOpenAIStickySessionOverflowEnabled)
			}
		})
	}
}
