//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestUpdateServiceImageModeRejectsBinaryMutations(t *testing.T) {
	for _, mode := range []string{"image", " IMAGE ", "invalid-mode"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_UPDATE_MODE", mode)
			// Nil clients ensure rejection happens before remote/cache access.
			svc := NewUpdateService(nil, nil, "0.2.23-pixcook.1", "release")
			for name, mutate := range map[string]func() error{
				"update":           func() error { return svc.PerformUpdate(context.Background()) },
				"local rollback":   svc.Rollback,
				"release rollback": func() error { return svc.RollbackToVersion(context.Background(), "0.2.22") },
			} {
				t.Run(name, func(t *testing.T) {
					err := mutate()
					require.Equal(t, http.StatusConflict, infraerrors.Code(err))
					require.Equal(t, "IMAGE_MANAGED_UPDATE_REQUIRED", infraerrors.Reason(err))
				})
			}
		})
	}
}

func TestUpdateServiceImageModeDoesNotOfferInheritedReleases(t *testing.T) {
	t.Setenv("SUB2API_UPDATE_MODE", "image")
	stale, err := json.Marshal(updateCacheData{
		Latest:      "0.2.24-klno.1",
		ReleaseInfo: &ReleaseInfo{Name: "KlN binary release", Assets: []Asset{{Name: "sub2api.tar.gz"}}},
		Timestamp:   time.Now().Unix(),
	})
	require.NoError(t, err)
	svc := NewUpdateService(&updateServiceCacheStub{data: string(stale)}, nil, "0.2.23-pixcook.1", "release")
	for _, force := range []bool{false, true} {
		info, err := svc.CheckUpdate(context.Background(), force)
		require.NoError(t, err)
		require.Equal(t, "0.2.23-pixcook.1", info.CurrentVersion)
		require.Equal(t, info.CurrentVersion, info.LatestVersion)
		require.False(t, info.HasUpdate)
		require.Nil(t, info.ReleaseInfo)
		require.Contains(t, info.Warning, "image")
		encoded, err := json.Marshal(info)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(encoded, &fields))
		require.Equal(t, "image", fields["update_mode"])
	}
	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Empty(t, versions, "KlN release binaries are not image rollback candidates")
}
