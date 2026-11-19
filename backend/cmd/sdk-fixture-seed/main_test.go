package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRejectsDirectoryWithoutFixtureMarker(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := run([]string{"--fixture-dir", dir, "--gateway-manifest", filepath.Join(dir, "gateway.json"), "--main-commit", "test-commit"}, &out)
	if err == nil || !strings.Contains(err.Error(), "fixture marker") {
		t.Fatalf("got %v, want missing fixture marker refusal", err)
	}
	if out.Len() != 0 {
		t.Fatal("refused fixture must not emit a manifest")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("refusal changed fixture directory: %v, %v", entries, err)
	}
}

func TestCLIRejectsRemoteGatewayBeforeDatabaseAccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := []byte(`{"contract_version":1,"run_id":"0123456789abcdef"}`)
	if err := os.WriteFile(filepath.Join(dir, ".bps-sdk-fixture.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dir)
	t.Setenv("CONFIG_FILE", filepath.Join(dir, "config.yaml"))
	gateway := map[string]any{"contract_version": 1, "source": "local", "seam": "gateway-diagnostic", "gateway_base_url": "https://example.invalid/v1", "bps_metrics_url": "http://127.0.0.1:1234/metrics"}
	data, _ := json.Marshal(gateway)
	path := filepath.Join(dir, "gateway.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := run([]string{"--fixture-dir", dir, "--gateway-manifest", path, "--fixture-out", filepath.Join(dir, "seed.json"), "--main-commit", "test"}, &out)
	if err == nil || !strings.Contains(err.Error(), "local loopback") {
		t.Fatalf("got %v, want remote gateway refusal", err)
	}
	if out.Len() != 0 {
		t.Fatal("refused fixture emitted output")
	}
	if _, err := os.Stat(filepath.Join(dir, "seed.json")); !os.IsNotExist(err) {
		t.Fatal("refused fixture wrote manifest")
	}
}
