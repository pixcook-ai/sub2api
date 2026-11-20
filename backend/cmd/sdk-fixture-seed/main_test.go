package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func managedSeedInputs(t *testing.T) (string, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		".bps-sdk-fixture.json": `{"contract_version":1,"run_id":"0123456789abcdef"}`,
		"gateway.key":           "public-fixture-key-0123456789abcdef\n",
		"service.key":           "private-service-key-0123456789abcdef\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DATA_DIR", dir)
	t.Setenv("CONFIG_FILE", filepath.Join(dir, "config.yaml"))
	description := map[string]any{
		"contract": "bps-files/1", "json_profile": "bps-files-json/1",
		"backend_id": "fixture-backend", "credential_generation": "fixture-generation",
		"subject_fingerprint": "sha256:" + strings.Repeat("a", 64),
		"limits": map[string]any{
			"file_bytes": 209715200, "message_bytes": 524288000, "message_positions": 20,
			"staging_bytes": 2147483648, "admission_bytes": 524288000,
			"concurrent_admissions": 4, "live_admissions": 32, "admission_ttl_seconds": 600,
			"manifest_bytes": 67108864, "unique_files": 128, "free_space_reserve_bytes": 1,
			"admit_timeout_seconds": 300, "ensure_timeout_seconds": 300,
		},
		"capabilities": map[string]any{"batch_admit": true, "ensure_upload": true, "metadata_recovery": false},
	}
	return dir, map[string]any{
		"contract_version": 1, "source": "local", "seam": "gateway-diagnostic",
		"gateway_base_url": "http://127.0.0.1:8790", "bps_metrics_url": "http://127.0.0.1:8791/metrics",
		"api_key_file": filepath.Join(dir, "gateway.key"),
		"managed_files": map[string]any{
			"contract": "bps-files/1", "json_profile": "bps-files-json/1",
			"control_url": "http://127.0.0.1:8790", "service_key_file": filepath.Join(dir, "service.key"),
			"backend_id": "fixture-backend", "credential_generation": "fixture-generation",
			"description": description,
		},
	}
}

func runManagedSeedBeforeConfig(t *testing.T, dir string, gateway map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(gateway)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gateway.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = run([]string{"--fixture-dir", dir, "--gateway-manifest", path, "--fixture-out", filepath.Join(dir, "seed.json"), "--main-commit", "test", "--require-managed-files"}, &out)
	if out.Len() != 0 {
		t.Fatal("pre-database validation emitted a manifest")
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "seed.json")); !os.IsNotExist(statErr) {
		t.Fatal("pre-database validation created output")
	}
	return err
}

func TestCLIManagedFixtureRequiresVerifiedDescriptionBeforeDatabaseAccess(t *testing.T) {
	dir, gateway := managedSeedInputs(t)
	delete(gateway, "managed_files")
	err := runManagedSeedBeforeConfig(t, dir, gateway)
	if err == nil || !strings.Contains(err.Error(), "managed fixture") {
		t.Fatalf("got %v, want missing managed fixture refusal", err)
	}
}

func TestCLIManagedFixtureAcceptsObservedDescriptionBeforeConfig(t *testing.T) {
	dir, gateway := managedSeedInputs(t)
	err := runManagedSeedBeforeConfig(t, dir, gateway)
	if err == nil || !strings.Contains(err.Error(), "fixture config") {
		t.Fatalf("got %v, want validated fixture to reach config boundary", err)
	}
}

func TestCLIManagedFixtureRejectsUnprovenPolicyBeforeDatabaseAccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(string, map[string]any)
	}{
		{"missing description", func(_ string, m map[string]any) { delete(m, "description") }},
		{"remote control", func(_ string, m map[string]any) { m["control_url"] = "https://example.invalid" }},
		{"other local origin", func(_ string, m map[string]any) { m["control_url"] = "http://127.0.0.1:8792" }},
		{"control path", func(_ string, m map[string]any) { m["control_url"] = "http://127.0.0.1:8790/v1" }},
		{"public service key", func(dir string, m map[string]any) { m["service_key_file"] = filepath.Join(dir, "gateway.key") }},
		{"shared service key", func(dir string, _ map[string]any) {
			if err := os.Chmod(filepath.Join(dir, "service.key"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"short service key", func(dir string, _ map[string]any) {
			if err := os.WriteFile(filepath.Join(dir, "service.key"), []byte("short"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink service key", func(dir string, m map[string]any) {
			path := filepath.Join(dir, "service-link.key")
			if err := os.Symlink(filepath.Join(dir, "service.key"), path); err != nil {
				t.Fatal(err)
			}
			m["service_key_file"] = path
		}},
		{"relative service key", func(_ string, m map[string]any) { m["service_key_file"] = "service.key" }},
		{"control zero port", func(_ string, m map[string]any) { m["control_url"] = "http://127.0.0.1:0" }},
		{"control fragment", func(_ string, m map[string]any) { m["control_url"] = "http://127.0.0.1:8790#fragment" }},
		{"control escaped path", func(_ string, m map[string]any) { m["control_url"] = "http://127.0.0.1:8790/%2f" }},
		{"null description string", func(_ string, m map[string]any) { m["description"].(map[string]any)["backend_id"] = nil }},
		{"fractional integer", func(_ string, m map[string]any) {
			m["description"].(map[string]any)["limits"].(map[string]any)["file_bytes"] = 34603027.5
		}},
		{"unknown capability", func(_ string, m map[string]any) {
			m["description"].(map[string]any)["capabilities"].(map[string]any)["unknown"] = true
		}},
		{"wrong contract", func(_ string, m map[string]any) { m["contract"] = "bps-files/2" }},
		{"wrong identity", func(_ string, m map[string]any) { m["description"].(map[string]any)["backend_id"] = "another-backend" }},
		{"unknown description field", func(_ string, m map[string]any) { m["description"].(map[string]any)["unknown"] = true }},
		{"case aliased field", func(_ string, m map[string]any) {
			d := m["description"].(map[string]any)
			d["Backend_Id"] = d["backend_id"]
			delete(d, "backend_id")
		}},
		{"missing recovery declaration", func(_ string, m map[string]any) {
			delete(m["description"].(map[string]any)["capabilities"].(map[string]any), "metadata_recovery")
		}},
		{"unsupported recovery", func(_ string, m map[string]any) {
			m["description"].(map[string]any)["capabilities"].(map[string]any)["metadata_recovery"] = true
		}},
		{"insufficient large file limit", func(_ string, m map[string]any) {
			m["description"].(map[string]any)["limits"].(map[string]any)["file_bytes"] = 33554432
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, gateway := managedSeedInputs(t)
			tc.alter(dir, gateway["managed_files"].(map[string]any))
			err := runManagedSeedBeforeConfig(t, dir, gateway)
			if err == nil || !strings.Contains(err.Error(), "managed fixture") {
				t.Fatalf("got %v, want managed fixture refusal before config or DB", err)
			}
			if strings.Contains(err.Error(), "private-service-key") {
				t.Fatal("validation leaked a service credential")
			}
		})
	}
}

func TestManagedFixtureAccountsUseActualExplicitCapabilityGate(t *testing.T) {
	managed := &managedFixture{
		ControlURL: "http://127.0.0.1:8790", ServiceKey: "private-service-key-0123456789abcdef",
		Description: managedfiles.DescribeResult{Contract: "bps-files/1", JSONProfile: "bps-files-json/1", Identity: managedfiles.Identity{
			BackendID: "fixture-backend", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "fixture-generation",
		}},
	}
	for _, scenario := range []string{"python/managed-file-json", "javascript/managed-file-large-sse", "python/managed-file-inline-json", "javascript/managed-file-inline-large-json"} {
		account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 5,
			Credentials: fixtureAccountCredentials("http://127.0.0.1:8790/v1", "public-key", scenario, managed),
			Extra:       map[string]any{"openai_responses_mode": "force_responses", "openai_responses_supported": true},
		}
		actual, err := service.BPSFilesConfigForAccount(account)
		if err != nil {
			t.Fatalf("%s did not pass the production capability gate: %v", scenario, err)
		}
		if actual.Expected != managed.Description.Identity || actual.ServiceKey != managed.ServiceKey || actual.ControlURL != managed.ControlURL {
			t.Fatal("actual account contract differs from the observed description")
		}
	}
	for _, scenario := range []string{"default", "python/failed-usage-sse", "javascript/files-list-pages"} {
		credentials := fixtureAccountCredentials("http://127.0.0.1:8790/v1", "public-key", scenario, managed)
		if len(credentials) != 2 {
			t.Fatalf("ordinary scenario %s received a private capability", scenario)
		}
	}
}

func TestStoredFixtureAccountsKeepIndependentLocalResponseCapability(t *testing.T) {
	managed := &managedFixture{ControlURL: "http://127.0.0.1:8790", ServiceKey: "private-service-key-0123456789abcdef",
		Description: managedfiles.DescribeResult{Identity: managedfiles.Identity{BackendID: "fixture-backend", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "fixture-generation"}}}
	for _, scenario := range []string{"python/stored-default-json", "javascript/stored-false-sse"} {
		credentials := fixtureAccountCredentials("http://127.0.0.1:8790/v1", "public-key", scenario, managed)
		if credentials["bps_responses_contract"] != "local-responses/1" || credentials["bps_responses_control_url"] != managed.ControlURL || credentials["bps_responses_service_key"] != managed.ServiceKey {
			t.Fatalf("stored scenario %s lacks the private local responses contract", scenario)
		}
		if credentials["bps_responses_backend_id"] != managed.Description.BackendID || credentials["bps_responses_subject_fingerprint"] != managed.Description.SubjectFingerprint || credentials["bps_responses_credential_generation"] != managed.Description.CredentialGeneration {
			t.Fatalf("stored scenario %s has a different backend identity", scenario)
		}
		capabilities, _ := credentials["openai_capabilities"].([]string)
		if strings.Join(capabilities, ",") != "bps_local_responses,chat_completions,responses" {
			t.Fatalf("stored scenario %s capabilities = %v", scenario, capabilities)
		}
		if _, exists := credentials["bps_files_contract"]; exists {
			t.Fatal("stored capability must not imply public managed Files support")
		}
	}
}

func TestStoredFixtureScenariosAppendWithoutChangingExistingPools(t *testing.T) {
	base := fixtureScenarios(true)
	original := append([]string(nil), base...)
	combined := appendStoredFixtureScenarios(base)
	if len(combined) != len(original)+14 {
		t.Fatalf("stored scenarios added %d entries, want 14", len(combined)-len(original))
	}
	for i, scenario := range original {
		if combined[i] != scenario {
			t.Fatalf("existing scenario %d changed from %s to %s", i, scenario, combined[i])
		}
	}
	for i, scenario := range []string{"stored-default-json", "stored-true-json", "stored-default-sse", "stored-true-sse", "stored-false-json", "stored-false-sse", "stored-lifecycle"} {
		if combined[len(original)+i] != "python/"+scenario || combined[len(original)+7+i] != "javascript/"+scenario {
			t.Fatalf("stored scenario %s is missing or assigned to a shared pool", scenario)
		}
	}
}

func TestInlineFixtureScenariosComposeAfterStoredWithoutChangingExistingPools(t *testing.T) {
	base := appendStoredFixtureScenarios(fixtureScenarios(true))
	original := append([]string(nil), base...)
	combined := appendInlineFixtureScenarios(base)
	if len(combined) != len(original)+14 {
		t.Fatalf("inline scenarios added %d entries, want 14", len(combined)-len(original))
	}
	for i, scenario := range original {
		if combined[i] != scenario {
			t.Fatalf("existing scenario %d changed from %s to %s", i, scenario, combined[i])
		}
	}
	for i, scenario := range []string{"managed-file-inline-json", "managed-file-inline-sse", "managed-file-inline-large-json", "managed-file-inline-mixed-json", "managed-file-inline-mixed-sse", "managed-file-inline-invalid-batch", "managed-file-inline-owner-deny"} {
		if combined[len(original)+i] != "python/"+scenario || combined[len(original)+7+i] != "javascript/"+scenario {
			t.Fatalf("inline scenario %s is missing or assigned to a shared pool", scenario)
		}
	}
}

func TestCLIStoredFixtureRequiresVerifiedControlPlaneBeforeDatabase(t *testing.T) {
	testOptionalFixtureRequiresVerifiedControlPlane(t, "--require-stored-responses")
}

func TestCLIInlineFixtureRequiresVerifiedControlPlaneBeforeDatabase(t *testing.T) {
	testOptionalFixtureRequiresVerifiedControlPlane(t, "--require-inline-files")
}

func testOptionalFixtureRequiresVerifiedControlPlane(t *testing.T, option string) {
	t.Helper()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "missing"}[missing], func(t *testing.T) {
			dir, gateway := managedSeedInputs(t)
			if missing {
				delete(gateway, "managed_files")
			}
			raw, err := json.Marshal(gateway)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "stored-gateway.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = run([]string{"--fixture-dir", dir, "--gateway-manifest", path, "--fixture-out", filepath.Join(dir, "seed.json"), "--main-commit", "test", option}, &out)
			want := "fixture config"
			if missing {
				want = "managed fixture"
			}
			if err == nil || !strings.Contains(err.Error(), want) || out.Len() != 0 {
				t.Fatalf("%s reached wrong setup boundary: %v, output bytes %d", option, err, out.Len())
			}
		})
	}
}

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
