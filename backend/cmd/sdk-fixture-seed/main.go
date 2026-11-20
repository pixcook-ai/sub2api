// sdk-fixture-seed creates synthetic resources only in an owned local fixture.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func privateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private regular file required: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || !os.SameFile(info, opened) {
		return nil, errors.New("private file changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("private fixture input exceeds limit")
	}
	return raw, nil
}
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(data)
	return err
}
func token() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func loopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }
func localURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || strings.TrimSpace(value) != value || u.Scheme != "http" || !loopback(u.Hostname()) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535
}

type managedFixture struct {
	ControlURL        string
	ServiceKey        string
	Description       managedfiles.DescribeResult
	DescriptionSHA256 string
}

func fixtureScenarios(managed bool) []string {
	base := []string{
		"upstream429-no-retry", "upstream429-default-retry", "truncated",
		"policy-rejections", "policy-catalog", "tool-strict-default",
		"failed-usage-sse", "failed-usage-json", "truncated-json",
		"incomplete-sse", "incomplete-json", "annotations-sse", "annotations-json",
	}
	if managed {
		base = append(base, "managed-file-json", "managed-file-sse", "managed-file-large-json", "managed-file-large-sse",
			"managed-file-owner-deny", "managed-file-othergroup-deny", "managed-file-private-header-forgery",
			"files-list-pages", "files-cursor-owner", "files-expiry-contract")
	}
	scenarios := []string{"default"}
	for _, sdk := range []string{"python", "javascript"} {
		for _, scenario := range base {
			scenarios = append(scenarios, sdk+"/"+scenario)
		}
	}
	return scenarios
}

func appendStoredFixtureScenarios(scenarios []string) []string {
	for _, sdk := range []string{"python", "javascript"} {
		for _, scenario := range []string{"stored-default-json", "stored-true-json", "stored-default-sse", "stored-true-sse", "stored-false-json", "stored-false-sse", "stored-lifecycle"} {
			scenarios = append(scenarios, sdk+"/"+scenario)
		}
	}
	return scenarios
}

func appendInlineFixtureScenarios(scenarios []string) []string {
	for _, sdk := range []string{"python", "javascript"} {
		for _, scenario := range []string{"managed-file-inline-json", "managed-file-inline-sse", "managed-file-inline-large-json", "managed-file-inline-mixed-json", "managed-file-inline-mixed-sse", "managed-file-inline-invalid-batch", "managed-file-inline-owner-deny"} {
			scenarios = append(scenarios, sdk+"/"+scenario)
		}
	}
	return scenarios
}

func appendRecoveryFixtureScenarios(scenarios []string) []string {
	for _, sdk := range []string{"python", "javascript"} {
		for _, scenario := range []string{"managed-recovery-json", "managed-recovery-sse", "managed-recovery-large-sse"} {
			scenarios = append(scenarios, sdk+"/"+scenario)
		}
	}
	return scenarios
}

func appendToolSearchFixtureScenarios(scenarios []string) []string {
	for _, sdk := range []string{"python", "javascript"} {
		for _, scenario := range []string{"tool-search-loop-json", "tool-search-loop-sse"} {
			scenarios = append(scenarios, sdk+"/"+scenario)
		}
	}
	return scenarios
}

func fixtureAccountCredentials(upstream, key, scenario string, managed *managedFixture) map[string]any {
	credentials := map[string]any{"base_url": upstream, "api_key": key}
	if managed != nil && (strings.Contains(scenario, "/managed-file-") || strings.Contains(scenario, "/managed-recovery-")) {
		credentials["openai_capabilities"] = []string{"bps_files", "chat_completions", "responses"}
		credentials["bps_files_contract"] = managed.Description.Contract
		credentials["bps_files_control_url"] = managed.ControlURL
		credentials["bps_files_service_key"] = managed.ServiceKey
		credentials["bps_files_backend_id"] = managed.Description.BackendID
		credentials["bps_files_subject_fingerprint"] = managed.Description.SubjectFingerprint
		credentials["bps_files_credential_generation"] = managed.Description.CredentialGeneration
	}
	if managed != nil && strings.Contains(scenario, "/stored-") {
		credentials["openai_capabilities"] = []string{"bps_local_responses", "chat_completions", "responses"}
		credentials["bps_responses_contract"] = "local-responses/1"
		credentials["bps_responses_control_url"] = managed.ControlURL
		credentials["bps_responses_service_key"] = managed.ServiceKey
		credentials["bps_responses_backend_id"] = managed.Description.BackendID
		credentials["bps_responses_subject_fingerprint"] = managed.Description.SubjectFingerprint
		credentials["bps_responses_credential_generation"] = managed.Description.CredentialGeneration
	}
	return credentials
}

type fixtureRoleKey struct {
	ID      int64  `json:"id"`
	UserID  int64  `json:"user_id"`
	GroupID int64  `json:"group_id"`
	Path    string `json:"-"`
}

func exactFixtureObject(value any, names ...string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(names) {
		return nil, false
	}
	for _, name := range names {
		if _, exists := object[name]; !exists {
			return nil, false
		}
	}
	return object, true
}

// The launcher authenticates Describe; the seed independently validates the
// resulting private snapshot before opening the isolated fixture database.
func validateManagedFixture(raw json.RawMessage, gatewayURL, publicKey string, required bool) (*managedFixture, error) {
	fail := func() (*managedFixture, error) { return nil, errors.New("invalid managed fixture metadata") }
	if len(raw) == 0 {
		if required {
			return fail()
		}
		return nil, nil
	}
	value, err := managedfiles.Parse(raw)
	if err != nil {
		return fail()
	}
	root, ok := exactFixtureObject(value, "contract", "json_profile", "control_url", "service_key_file", "backend_id", "credential_generation", "description")
	if !ok {
		return fail()
	}
	fields := map[string]string{}
	for _, name := range []string{"contract", "json_profile", "control_url", "service_key_file", "backend_id", "credential_generation"} {
		field, ok := root[name].(string)
		if !ok || field == "" || strings.TrimSpace(field) != field {
			return fail()
		}
		fields[name] = field
	}
	control, err := url.Parse(fields["control_url"])
	upstream, _ := url.Parse(gatewayURL)
	if err != nil || !localURL(fields["control_url"]) || control.Path != "" || control.Scheme != upstream.Scheme || control.Host != upstream.Host ||
		fields["contract"] != managedfiles.Contract || fields["json_profile"] != managedfiles.JSONProfile || !filepath.IsAbs(fields["service_key_file"]) {
		return fail()
	}
	secret, err := privateFile(fields["service_key_file"])
	serviceKey := strings.TrimSpace(string(secret))
	if err != nil || len(secret) > 1025 || len(serviceKey) < 32 || len(serviceKey) > 1024 || serviceKey == publicKey {
		return fail()
	}
	for _, b := range []byte(serviceKey) {
		if b < 33 || b > 126 {
			return fail()
		}
	}
	for _, name := range []string{"backend_id", "credential_generation"} {
		if !regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`).MatchString(fields[name]) {
			return fail()
		}
	}
	description, ok := exactFixtureObject(root["description"], "contract", "json_profile", "backend_id", "subject_fingerprint", "credential_generation", "limits", "capabilities")
	if !ok {
		return fail()
	}
	for _, field := range []string{"contract", "json_profile", "backend_id", "subject_fingerprint", "credential_generation"} {
		if _, ok := description[field].(string); !ok {
			return fail()
		}
	}
	limits, ok := exactFixtureObject(description["limits"], "file_bytes", "message_bytes", "message_positions", "staging_bytes", "admission_bytes", "concurrent_admissions", "live_admissions", "admission_ttl_seconds", "manifest_bytes", "unique_files", "free_space_reserve_bytes", "admit_timeout_seconds", "ensure_timeout_seconds")
	if !ok {
		return fail()
	}
	for _, value := range limits {
		number, ok := value.(json.Number)
		if !ok {
			return fail()
		}
		if _, err := number.Int64(); err != nil {
			return fail()
		}
	}
	capabilities, ok := exactFixtureObject(description["capabilities"], "batch_admit", "ensure_upload", "metadata_recovery")
	if !ok {
		return fail()
	}
	for _, field := range []string{"batch_admit", "ensure_upload", "metadata_recovery"} {
		if _, ok := capabilities[field].(bool); !ok {
			return fail()
		}
	}
	canonical, err := json.Marshal(description)
	if err != nil {
		return fail()
	}
	var observed managedfiles.DescribeResult
	if json.Unmarshal(canonical, &observed) != nil || observed.Contract != managedfiles.Contract || observed.JSONProfile != managedfiles.JSONProfile ||
		observed.BackendID != fields["backend_id"] || observed.CredentialGeneration != fields["credential_generation"] || managedfiles.ValidateIdentity(observed.Identity) != nil ||
		!observed.Capabilities.BatchAdmit || !observed.Capabilities.EnsureUpload || observed.Capabilities.MetadataRecovery {
		return fail()
	}
	l := observed.Limits
	if l.FileBytes < 34603027 || l.FileBytes > managedfiles.MaxFileBytes || l.MessageBytes < l.FileBytes || l.MessageBytes > managedfiles.MaxMessageBytes ||
		l.MessagePositions < 1 || l.MessagePositions > managedfiles.MaxMessagePositions || l.StagingBytes < l.AdmissionBytes || l.StagingBytes > 16<<30 ||
		l.AdmissionBytes < l.FileBytes || l.AdmissionBytes > 2<<30 || l.ConcurrentAdmissions < 1 || l.ConcurrentAdmissions > 32 ||
		l.LiveAdmissions < l.ConcurrentAdmissions || l.LiveAdmissions > 256 || l.AdmissionTTLSeconds < 1 || l.AdmissionTTLSeconds > 3600 ||
		l.ManifestBytes < 1 || l.ManifestBytes > managedfiles.MaxJSONBytes || l.UniqueFiles < 1 || l.UniqueFiles > managedfiles.MaxUniqueFiles ||
		l.FreeSpaceReserveBytes < 1 || l.FreeSpaceReserveBytes > 16<<30 || l.AdmitTimeoutSeconds < 1 || l.AdmitTimeoutSeconds > 3600 || l.EnsureTimeoutSeconds < 1 || l.EnsureTimeoutSeconds > 3600 {
		return fail()
	}
	digest := sha256.Sum256(canonical)
	return &managedFixture{ControlURL: fields["control_url"], ServiceKey: serviceKey, Description: observed, DescriptionSHA256: hex.EncodeToString(digest[:])}, nil
}

func validateFixtureRestart(raw json.RawMessage) error {
	fail := func() error { return errors.New("invalid recovery fixture restart metadata") }
	value, err := managedfiles.Parse(raw)
	if err != nil {
		return fail()
	}
	root, ok := exactFixtureObject(value, "contract", "control_socket", "supervisor_pid")
	if !ok || root["contract"] != "bps-fixture-gateway-restart/1" {
		return fail()
	}
	pid, ok := root["supervisor_pid"].(json.Number)
	if !ok {
		return fail()
	}
	n, err := pid.Int64()
	if err != nil || n <= 0 || n > 2147483647 {
		return fail()
	}
	path, ok := root["control_socket"].(string)
	if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fail()
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return fail()
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Type() != os.ModeSocket || info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fail()
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(owner.Uid) != uint64(os.Getuid()) {
		return fail()
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 || parent.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fail()
	}
	owner, ok = parent.Sys().(*syscall.Stat_t)
	if !ok || uint64(owner.Uid) != uint64(os.Getuid()) {
		return fail()
	}
	return nil
}

func validateRecoveryFixture(raw, restart json.RawMessage, managed *managedFixture, required bool) (map[string]any, error) {
	fail := func() (map[string]any, error) { return nil, errors.New("invalid metadata recovery fixture snapshot") }
	if len(raw) == 0 && !required {
		return nil, nil
	}
	if managed == nil || managed.Description.Capabilities.MetadataRecovery || validateFixtureRestart(restart) != nil {
		return fail()
	}
	value, err := managedfiles.Parse(raw)
	if err != nil {
		return fail()
	}
	root, ok := exactFixtureObject(value, "contract", "identity", "description_sha256", "description")
	if !ok || root["contract"] != "bps-metadata-recovery/1" {
		return fail()
	}
	digest, ok := root["description_sha256"].(string)
	if !ok || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
		return fail()
	}
	expected := managed.Description
	expected.Capabilities.MetadataRecovery = true
	expectedRaw, err := json.Marshal(expected)
	if err != nil {
		return fail()
	}
	expectedDescription, err := managedfiles.Parse(expectedRaw)
	if err != nil || !reflect.DeepEqual(root["description"], expectedDescription) {
		return fail()
	}
	identityRaw, err := json.Marshal(managed.Description.Identity)
	if err != nil {
		return fail()
	}
	identity, err := managedfiles.Parse(identityRaw)
	if err != nil || !reflect.DeepEqual(root["identity"], identity) {
		return fail()
	}
	canonical, err := json.Marshal(root["description"])
	if err != nil {
		return fail()
	}
	actual := sha256.Sum256(canonical)
	if hex.EncodeToString(actual[:]) != digest {
		return fail()
	}
	return map[string]any{"contract": "bps-metadata-recovery/1", "identity": managed.Description.Identity, "description_sha256": digest}, nil
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sdk-fixture-seed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("fixture-dir", "", "")
	gatewayPath := fs.String("gateway-manifest", "", "")
	model := fs.String("model", "gpt-6-astra", "")
	commit := fs.String("main-commit", "", "")
	output := fs.String("fixture-out", "", "")
	requireManaged := fs.Bool("require-managed-files", false, "")
	requireStored := fs.Bool("require-stored-responses", false, "")
	requireInline := fs.Bool("require-inline-files", false, "")
	requireRecovery := fs.Bool("require-metadata-recovery", false, "")
	requireToolSearch := fs.Bool("require-tool-search", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	markerData, err := privateFile(filepath.Join(*dir, ".bps-sdk-fixture.json"))
	if err != nil {
		return fmt.Errorf("fixture marker: %w", err)
	}
	var marker struct {
		Version int    `json:"contract_version"`
		RunID   string `json:"run_id"`
	}
	if json.Unmarshal(markerData, &marker) != nil || marker.Version != 1 || !regexp.MustCompile(`^[a-f0-9]{16}$`).MatchString(marker.RunID) {
		return errors.New("invalid fixture marker")
	}
	info, err := os.Lstat(*dir)
	if err != nil || !filepath.IsAbs(*dir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("fixture directory must be absolute and private")
	}
	if os.Getenv("DATA_DIR") != *dir || os.Getenv("CONFIG_FILE") != filepath.Join(*dir, "config.yaml") {
		return errors.New("fixture DATA_DIR/CONFIG_FILE mismatch")
	}
	if *output == "" || filepath.Dir(*output) != *dir || *commit == "" || *model == "" {
		return errors.New("fixture output, commit and model required")
	}
	if _, err := os.Lstat(*output); !os.IsNotExist(err) {
		return errors.New("fixture output already exists")
	}
	var gateway struct {
		Version      int             `json:"contract_version"`
		Source       string          `json:"source"`
		Seam         string          `json:"seam"`
		BaseURL      string          `json:"gateway_base_url"`
		MetricsURL   string          `json:"bps_metrics_url"`
		KeyFile      string          `json:"api_key_file"`
		Commit       string          `json:"gateway_commit"`
		ManagedFiles json.RawMessage `json:"managed_files"`
		Recovery     json.RawMessage `json:"metadata_recovery"`
		Restart      json.RawMessage `json:"gateway_restart"`
	}
	data, err := privateFile(*gatewayPath)
	if err != nil {
		return err
	}
	if _, err = managedfiles.Parse(data); err != nil {
		return errors.New("invalid private gateway manifest")
	}
	if err = json.Unmarshal(data, &gateway); err != nil {
		return errors.New("invalid private gateway manifest")
	}
	if gateway.Version != 1 || gateway.Source != "local" || gateway.Seam != "gateway-diagnostic" || !localURL(gateway.BaseURL) || !localURL(gateway.MetricsURL) {
		return errors.New("gateway must be a local loopback diagnostic fixture")
	}
	gatewayURL, _ := url.Parse(gateway.BaseURL)
	if gatewayURL.Path != "" && gatewayURL.Path != "/v1" {
		return errors.New("invalid gateway path")
	}
	key, err := privateFile(gateway.KeyFile)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(key))) == 0 {
		return errors.New("empty gateway key")
	}
	managed, err := validateManagedFixture(gateway.ManagedFiles, gateway.BaseURL, strings.TrimSpace(string(key)), *requireManaged || *requireStored || *requireInline || *requireRecovery)
	if err != nil {
		return err
	}
	recovery, err := validateRecoveryFixture(gateway.Recovery, gateway.Restart, managed, *requireRecovery)
	if err != nil {
		return err
	}
	if _, err := privateFile(filepath.Join(*dir, "config.yaml")); err != nil {
		return fmt.Errorf("fixture config: %w", err)
	}
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		return err
	}
	if cfg.RunMode != "standard" || !loopback(cfg.Database.Host) || !loopback(cfg.Redis.Host) || !loopback(cfg.Server.Host) || cfg.Database.DBName != "bps_sdk_"+marker.RunID {
		return errors.New("refuse nonisolated database/server configuration")
	}
	client, sqlDB, err := repository.InitEnt(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	for _, table := range []string{"users", "accounts", "api_keys"} {
		var count int
		if err := sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("fixture database is not fresh: %s", table)
		}
	}
	groups := repository.NewGroupRepository(client, sqlDB)
	users := repository.NewUserRepository(client, sqlDB)
	keys := repository.NewAPIKeyRepository(client, sqlDB)
	accounts := repository.NewAdminAccountRepository(client, sqlDB, nil)
	// Keep each SDK scenario in its own scheduling pool. Catalog steps deliberately
	// share one case/pool so a rejected update must preserve its prior session.
	scenarios := fixtureScenarios(managed != nil)
	if *requireStored {
		scenarios = appendStoredFixtureScenarios(scenarios)
	}
	if *requireInline {
		scenarios = appendInlineFixtureScenarios(scenarios)
	}
	if *requireRecovery {
		scenarios = appendRecoveryFixtureScenarios(scenarios)
	}
	if *requireToolSearch {
		scenarios = appendToolSearchFixtureScenarios(scenarios)
	}
	groupIDs := make([]int64, 0, len(scenarios))
	groupByScenario := map[string]int64{}
	accountIDs := []int64{}
	accountByScenario := map[string]int64{}
	upstreamBase := strings.TrimRight(gateway.BaseURL, "/")
	if gatewayURL.Path == "" {
		upstreamBase += "/v1"
	}
	for _, scenario := range scenarios {
		group := &service.Group{Name: "bps-sdk-" + marker.RunID + "-" + strings.ReplaceAll(scenario, "/", "-"), Platform: service.PlatformOpenAI, Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{*model}}}
		if err := groups.Create(ctx, group); err != nil {
			return err
		}
		groupIDs = append(groupIDs, group.ID)
		groupByScenario[scenario] = group.ID
		account := &service.Account{Name: group.Name, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: fixtureAccountCredentials(upstreamBase, strings.TrimSpace(string(key)), scenario, managed), Extra: map[string]any{"openai_responses_mode": "force_responses", "openai_responses_supported": true}, Status: service.StatusActive, Schedulable: true, Concurrency: 5, Priority: 1}
		if err := accounts.CreateWithAccountGroups(ctx, account, []service.AccountGroup{{GroupID: group.ID, Priority: 1}}); err != nil {
			return err
		}
		accountIDs = append(accountIDs, account.ID)
		accountByScenario[scenario] = account.ID
	}
	var userIDs []int64
	for _, suffix := range []string{"a", "b"} {
		user := &service.User{Email: marker.RunID + "-" + suffix + "@example.invalid", Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 5, AllowedGroups: groupIDs, RestrictPublicGroups: true}
		if err := user.SetPassword(token()); err != nil {
			return err
		}
		if err := users.Create(ctx, user); err != nil {
			return err
		}
		userIDs = append(userIDs, user.ID)
	}
	makeKey := func(name string, userID, groupID int64) (fixtureRoleKey, error) {
		value := "sk-fixture-" + token()
		key := &service.APIKey{UserID: userID, GroupID: &groupID, Key: value, Name: name, Status: service.StatusActive}
		if err := keys.Create(ctx, key); err != nil {
			return fixtureRoleKey{}, err
		}
		path := filepath.Join(*dir, name+".key")
		return fixtureRoleKey{ID: key.ID, UserID: userID, GroupID: groupID, Path: path}, writePrivate(path, []byte(value+"\n"))
	}
	first, err := makeKey("user-a1", userIDs[0], groupIDs[0])
	if err != nil {
		return err
	}
	same, err := makeKey("user-a2", userIDs[0], groupIDs[0])
	if err != nil {
		return err
	}
	other, err := makeKey("user-b", userIDs[1], groupIDs[0])
	if err != nil {
		return err
	}
	scenarioKeys := map[string]string{}
	scenarioRoleFiles := map[string]map[string]string{}
	scenarioRoleKeys := map[string]map[string]fixtureRoleKey{}
	for _, scenario := range scenarios[1:] {
		name := strings.ReplaceAll(scenario, "/", "-")
		groupID := groupByScenario[scenario]
		a1, err := makeKey(name, userIDs[0], groupID)
		if err != nil {
			return err
		}
		scenarioKeys[scenario] = a1.Path
		if !strings.Contains(scenario, "/tool-search-") && (managed == nil || (!strings.Contains(scenario, "/managed-file-") && !strings.Contains(scenario, "/managed-recovery-") && !strings.Contains(scenario, "/files-") && !strings.Contains(scenario, "/stored-"))) {
			continue
		}
		roleKeys := map[string]fixtureRoleKey{"a1": a1}
		for _, role := range []struct {
			name            string
			userID, groupID int64
		}{
			{"a2", userIDs[0], groupID}, {"b", userIDs[1], groupID}, {"c", userIDs[0], groupIDs[0]},
		} {
			created, err := makeKey(name+"-"+role.name, role.userID, role.groupID)
			if err != nil {
				return err
			}
			roleKeys[role.name] = created
		}
		roleFiles := map[string]string{}
		for role, key := range roleKeys {
			roleFiles[role] = key.Path
		}
		scenarioRoleFiles[scenario], scenarioRoleKeys[scenario] = roleFiles, roleKeys
	}
	manifest := map[string]any{"contract_version": 1, "source": "local", "seam": "main-auth-scheduler-gateway-bps", "main_base_url": fmt.Sprintf("http://127.0.0.1:%d/v1", cfg.Server.Port), "api_key_file": first.Path, "same_user_key_file": same.Path, "other_user_key_file": other.Path, "scenario_api_key_files": scenarioKeys, "main_commit": *commit, "gateway_commit": gateway.Commit, "gateway_base_url": gateway.BaseURL, "bps_metrics_url": gateway.MetricsURL, "model": *model, "data_dir": *dir, "resources": map[string]any{"users": userIDs, "groups": groupIDs, "accounts": accountIDs, "scenario_groups": groupByScenario, "scenario_accounts": accountByScenario, "scenario_role_keys": scenarioRoleKeys}}
	if managed != nil || *requireToolSearch {
		manifest["scenario_role_key_files"] = scenarioRoleFiles
	}
	if managed != nil {
		manifest["managed_files"] = map[string]any{"contract": managed.Description.Contract, "json_profile": managed.Description.JSONProfile, "identity": managed.Description.Identity, "description_sha256": managed.DescriptionSHA256}
	}
	if *requireStored {
		manifest["stored_responses"] = map[string]any{"contract": "local-responses/1", "identity": managed.Description.Identity,
			"scope": "plain-text-local-storage", "preflight": "required-not-yet-accepted"}
	}
	if *requireInline {
		manifest["inline_files"] = map[string]any{"contract": "bps-inline-files/1", "identity": managed.Description.Identity,
			"scope": "request-private-file-data"}
	}
	if *requireRecovery {
		manifest["metadata_recovery"] = recovery
	}
	if *requireToolSearch {
		manifest["tool_search"] = map[string]any{"contract": "bps-client-tool-search/1"}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(*output, append(encoded, '\n')); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, *output)
	return err
}
