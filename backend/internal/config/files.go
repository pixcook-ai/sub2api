package config

import (
	"fmt"
	"path/filepath"
)

// FilesConfig bounds the private, persistent storage used by the Files API.
type FilesConfig struct {
	Enabled               bool   `mapstructure:"enabled"`
	Root                  string `mapstructure:"root"`
	MaxFileBytes          int64  `mapstructure:"max_file_bytes"`
	UserQuotaBytes        int64  `mapstructure:"user_quota_bytes"`
	GlobalQuotaBytes      int64  `mapstructure:"global_quota_bytes"`
	FreeSpaceReserveBytes int64  `mapstructure:"free_space_reserve_bytes"`
	LeaseSeconds          int    `mapstructure:"lease_seconds"`
}

func DefaultFilesConfig() FilesConfig {
	return FilesConfig{
		MaxFileBytes:          200 << 20,
		UserQuotaBytes:        2 << 30,
		GlobalQuotaBytes:      20 << 30,
		FreeSpaceReserveBytes: 1 << 30,
		LeaseSeconds:          300,
	}
}

func (c FilesConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Root == "" || !filepath.IsAbs(c.Root) {
		return fmt.Errorf("gateway.files.root must be an explicit absolute persistent directory")
	}
	if c.MaxFileBytes <= 0 || c.MaxFileBytes > 200<<20 {
		return fmt.Errorf("gateway.files.max_file_bytes must be positive and no greater than 200 MiB")
	}
	if c.UserQuotaBytes < c.MaxFileBytes {
		return fmt.Errorf("gateway.files.user_quota_bytes must be finite and no less than max_file_bytes")
	}
	if c.GlobalQuotaBytes < c.UserQuotaBytes {
		return fmt.Errorf("gateway.files.global_quota_bytes must be finite and no less than user_quota_bytes")
	}
	if c.FreeSpaceReserveBytes <= 0 {
		return fmt.Errorf("gateway.files.free_space_reserve_bytes must be positive")
	}
	if c.LeaseSeconds < 60 {
		return fmt.Errorf("gateway.files.lease_seconds must be at least 60")
	}
	return nil
}
