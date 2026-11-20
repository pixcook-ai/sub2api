package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogFromServiceAdmin_RouteDegradedTriStateAdminOnly(t *testing.T) {
	t.Parallel()

	healthy, degraded := false, true
	for _, tc := range []struct {
		name     string
		value    *bool
		wantJSON string
	}{
		{name: "nil_tibo_not_applied", value: nil},
		{name: "false_healthy_route", value: &healthy, wantJSON: `"route_degraded":false`},
		{name: "true_http_fallback", value: &degraded, wantJSON: `"route_degraded":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log := &service.UsageLog{
				RequestID:     "req_tibo_route",
				Model:         "gpt-5.4",
				RouteDegraded: tc.value,
			}

			adminDTO := UsageLogFromServiceAdmin(log)
			require.Equal(t, tc.value, adminDTO.RouteDegraded)

			adminJSON, err := json.Marshal(adminDTO)
			require.NoError(t, err)
			if tc.wantJSON == "" {
				require.NotContains(t, string(adminJSON), `"route_degraded"`)
			} else {
				require.Contains(t, string(adminJSON), tc.wantJSON)
			}

			userJSON, err := json.Marshal(UsageLogFromService(log))
			require.NoError(t, err)
			require.NotContains(t, string(userJSON), `"route_degraded"`)
		})
	}
}
