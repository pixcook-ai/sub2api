package config

import (
	"strings"
	"testing"
)

func TestFilesConfigDisabledNeedsNoStorage(t *testing.T) {
	cfg := DefaultFilesConfig()
	if cfg.Enabled {
		t.Fatal("Files must be disabled by default")
	}
	if cfg.Root != "" {
		t.Fatal("Files storage requires an explicit persistent directory")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled Files must not require storage: %v", err)
	}
}

func TestFilesConfigEnabledRequiresBoundedPersistentStorage(t *testing.T) {
	valid := DefaultFilesConfig()
	valid.Enabled = true
	valid.Root = t.TempDir()
	if err := valid.Validate(); err != nil {
		t.Fatalf("explicit storage with finite quotas must be valid: %v", err)
	}

	for _, tc := range []struct {
		name string
		edit func(*FilesConfig)
		key  string
	}{
		{"missing root", func(c *FilesConfig) { c.Root = "" }, "root"},
		{"relative root", func(c *FilesConfig) { c.Root = "data/files" }, "root"},
		{"zero file limit", func(c *FilesConfig) { c.MaxFileBytes = 0 }, "max_file_bytes"},
		{"file over 200 MiB", func(c *FilesConfig) { c.MaxFileBytes = 209715201 }, "max_file_bytes"},
		{"zero user quota", func(c *FilesConfig) { c.UserQuotaBytes = 0 }, "user_quota_bytes"},
		{"user quota below file limit", func(c *FilesConfig) { c.UserQuotaBytes = 209715199 }, "user_quota_bytes"},
		{"zero global quota", func(c *FilesConfig) { c.GlobalQuotaBytes = 0 }, "global_quota_bytes"},
		{"global quota below user quota", func(c *FilesConfig) { c.GlobalQuotaBytes = 2147483647 }, "global_quota_bytes"},
		{"zero space reserve", func(c *FilesConfig) { c.FreeSpaceReserveBytes = 0 }, "free_space_reserve_bytes"},
		{"negative space reserve", func(c *FilesConfig) { c.FreeSpaceReserveBytes = -1 }, "free_space_reserve_bytes"},
		{"lease too short", func(c *FilesConfig) { c.LeaseSeconds = 59 }, "lease_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "gateway.files."+tc.key) {
				t.Fatalf("want actionable %s validation error, got %v", tc.key, err)
			}
		})
	}
}

func TestLoadFilesStoragePolicy(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	files := cfg.Gateway.Files
	if files.Enabled || files.Root != "" || files.MaxFileBytes != 209715200 ||
		files.UserQuotaBytes != 2147483648 || files.GlobalQuotaBytes != 21474836480 ||
		files.FreeSpaceReserveBytes != 1073741824 || files.LeaseSeconds != 300 {
		t.Fatalf("unsafe or unexpected Files defaults: %+v", files)
	}

	t.Setenv("GATEWAY_FILES_ENABLED", "true")
	t.Setenv("GATEWAY_FILES_ROOT", t.TempDir())
	t.Setenv("GATEWAY_FILES_GLOBAL_QUOTA_BYTES", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "gateway.files.global_quota_bytes") {
		t.Fatalf("enabled Files with explicit unlimited quota must fail startup: %v", err)
	}
}
