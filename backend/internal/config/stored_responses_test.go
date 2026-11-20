package config

import (
	"strings"
	"testing"
)

func TestLoadStoredResponsesDefaultAndExplicitBudgets(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Gateway.StoredResponses
	if c.Enabled || c.RetentionDays != 30 || c.LeaseSeconds != 120 || c.MaxResponseBytes != 1048576 || c.UserMaxRows != 1000 || c.UserMaxBytes != 67108864 || c.GlobalMaxRows != 100000 || c.GlobalMaxBytes != 1073741824 {
		t.Fatalf("unsafe storage defaults: %+v", c)
	}
	t.Setenv("GATEWAY_STORED_RESPONSES_ENABLED", "true")
	t.Setenv("GATEWAY_STORED_RESPONSES_RETENTION_DAYS", "7")
	t.Setenv("GATEWAY_STORED_RESPONSES_LEASE_SECONDS", "30")
	t.Setenv("GATEWAY_STORED_RESPONSES_MAX_RESPONSE_BYTES", "4096")
	t.Setenv("GATEWAY_STORED_RESPONSES_USER_MAX_ROWS", "10")
	t.Setenv("GATEWAY_STORED_RESPONSES_USER_MAX_BYTES", "8192")
	t.Setenv("GATEWAY_STORED_RESPONSES_GLOBAL_MAX_ROWS", "20")
	t.Setenv("GATEWAY_STORED_RESPONSES_GLOBAL_MAX_BYTES", "16384")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	c = cfg.Gateway.StoredResponses
	if !c.Enabled || c.RetentionDays != 7 || c.LeaseSeconds != 30 || c.MaxResponseBytes != 4096 || c.UserMaxRows != 10 || c.UserMaxBytes != 8192 || c.GlobalMaxRows != 20 || c.GlobalMaxBytes != 16384 {
		t.Fatalf("explicit storage budgets lost: %+v", c)
	}
	t.Setenv("GATEWAY_STORED_RESPONSES_GLOBAL_MAX_BYTES", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "gateway.stored_responses.global_max_bytes") {
		t.Fatalf("unlimited storage accepted: %v", err)
	}
}

func TestStoredResponsesConfigRejectsUnboundedStorage(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*StoredResponsesConfig)
		key  string
	}{
		{"retention below one day", func(c *StoredResponsesConfig) { c.RetentionDays = 0 }, "retention_days"},
		{"retention over thirty days", func(c *StoredResponsesConfig) { c.RetentionDays = 31 }, "retention_days"},
		{"lease too short", func(c *StoredResponsesConfig) { c.LeaseSeconds = 5 }, "lease_seconds"},
		{"lease too long", func(c *StoredResponsesConfig) { c.LeaseSeconds = 3601 }, "lease_seconds"},
		{"zero response limit", func(c *StoredResponsesConfig) { c.MaxResponseBytes = 0 }, "max_response_bytes"},
		{"oversized response limit", func(c *StoredResponsesConfig) { c.MaxResponseBytes = 67108865 }, "max_response_bytes"},
		{"zero user rows", func(c *StoredResponsesConfig) { c.UserMaxRows = 0 }, "user_max_rows"},
		{"global rows below user", func(c *StoredResponsesConfig) { c.GlobalMaxRows = 999 }, "global_max_rows"},
		{"user bytes below response", func(c *StoredResponsesConfig) { c.UserMaxBytes = 1048575 }, "user_max_bytes"},
		{"global bytes below user", func(c *StoredResponsesConfig) { c.GlobalMaxBytes = 67108863 }, "global_max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultStoredResponsesConfig()
			c.Enabled = true
			tc.edit(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "gateway.stored_responses."+tc.key) {
				t.Fatalf("invalid storage budget accepted: %v", err)
			}
		})
	}
}
