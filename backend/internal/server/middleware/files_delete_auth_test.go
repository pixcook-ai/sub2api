//go:build unit

package middleware

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestFilesDeleteResourceAuthAllowsExhaustedBalance(t *testing.T) {
	key := storedResponseAuthKeyForTest()
	key.Status, key.User.Balance = service.StatusAPIKeyQuotaExhausted, 0
	status, callback := storedResponseAuthRequestForTest(t, config.RunModeStandard, key, nil, http.MethodDelete, "/v1/files/file-owned")
	if status != http.StatusOK || callback {
		t.Fatalf("DELETE must use live resource auth without inference billing/upload publication: status=%d callback=%t", status, callback)
	}
}

func TestFilesDeleteResourceAuthKeepsStrictIdentityAndPathBoundaries(t *testing.T) {
	for _, mode := range []string{config.RunModeStandard, config.RunModeSimple} {
		t.Run(mode, func(t *testing.T) {
			for _, stateExpired := range []bool{false, true} {
				key := storedResponseAuthKeyForTest()
				if stateExpired {
					key.Status = service.StatusAPIKeyExpired
				} else {
					past := time.Now().Add(-time.Hour)
					key.ExpiresAt = &past
				}
				status, _ := storedResponseAuthRequestForTest(t, mode, key, nil, http.MethodDelete, "/v1/files/file-owned")
				if status != http.StatusForbidden {
					t.Fatalf("expired identity reached deletion in %s: %d", mode, status)
				}
			}
			key := storedResponseAuthKeyForTest()
			key.Group.SubscriptionType = service.SubscriptionTypeSubscription
			status, _ := storedResponseAuthRequestForTest(t, mode, key, nil, http.MethodDelete, "/v1/files/file-owned")
			if status != http.StatusServiceUnavailable {
				t.Fatalf("missing membership authorization failed open in %s: %d", mode, status)
			}
		})
	}
	for _, path := range []string{"/v1/files", "/v1/files/", "/v1/files/file-id/content", "/v1/files/file-id/extra", "/v1/files/file-id/"} {
		key := storedResponseAuthKeyForTest()
		key.Status, key.User.Balance = service.StatusAPIKeyQuotaExhausted, 0
		status, _ := storedResponseAuthRequestForTest(t, config.RunModeStandard, key, nil, http.MethodDelete, path)
		if status != http.StatusTooManyRequests {
			t.Fatalf("unsupported DELETE path %s bypassed billing/resource boundary: %d", path, status)
		}
	}
}
