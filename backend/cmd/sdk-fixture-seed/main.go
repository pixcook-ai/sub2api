// sdk-fixture-seed creates synthetic resources only in an owned local fixture.
package main

import (
	"context"
	"crypto/rand"
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
	"regexp"
	"strings"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
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
	return os.ReadFile(path)
}
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
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
	return err == nil && u.Scheme == "http" && loopback(u.Hostname()) && u.Port() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sdk-fixture-seed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("fixture-dir", "", "")
	gatewayPath := fs.String("gateway-manifest", "", "")
	model := fs.String("model", "gpt-6-astra", "")
	commit := fs.String("main-commit", "", "")
	output := fs.String("fixture-out", "", "")
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
		Version    int    `json:"contract_version"`
		Source     string `json:"source"`
		Seam       string `json:"seam"`
		BaseURL    string `json:"gateway_base_url"`
		MetricsURL string `json:"bps_metrics_url"`
		KeyFile    string `json:"api_key_file"`
		Commit     string `json:"gateway_commit"`
	}
	data, err := privateFile(*gatewayPath)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &gateway); err != nil {
		return err
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
	defer client.Close()
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
	scenarios := []string{"default", "python/upstream429-no-retry", "python/upstream429-default-retry", "python/truncated", "javascript/upstream429-no-retry", "javascript/upstream429-default-retry", "javascript/truncated"}
	groupIDs := make([]int64, 0, len(scenarios))
	groupByScenario := map[string]int64{}
	accountIDs := []int64{}
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
		account := &service.Account{Name: group.Name, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"base_url": upstreamBase, "api_key": strings.TrimSpace(string(key))}, Extra: map[string]any{"openai_responses_mode": "force_responses", "openai_responses_supported": true}, Status: service.StatusActive, Schedulable: true, Concurrency: 5, Priority: 1}
		if err := accounts.CreateWithAccountGroups(ctx, account, []service.AccountGroup{{GroupID: group.ID, Priority: 1}}); err != nil {
			return err
		}
		accountIDs = append(accountIDs, account.ID)
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
	makeKey := func(name string, userID, groupID int64) (string, error) {
		value := "sk-fixture-" + token()
		key := &service.APIKey{UserID: userID, GroupID: &groupID, Key: value, Name: name, Status: service.StatusActive}
		if err := keys.Create(ctx, key); err != nil {
			return "", err
		}
		path := filepath.Join(*dir, name+".key")
		return path, writePrivate(path, []byte(value+"\n"))
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
	for _, scenario := range scenarios[1:] {
		path, err := makeKey(strings.ReplaceAll(scenario, "/", "-"), userIDs[0], groupByScenario[scenario])
		if err != nil {
			return err
		}
		scenarioKeys[scenario] = path
	}
	manifest := map[string]any{"contract_version": 1, "source": "local", "seam": "main-auth-scheduler-gateway-bps", "main_base_url": fmt.Sprintf("http://127.0.0.1:%d/v1", cfg.Server.Port), "api_key_file": first, "same_user_key_file": same, "other_user_key_file": other, "scenario_api_key_files": scenarioKeys, "main_commit": *commit, "gateway_commit": gateway.Commit, "gateway_base_url": gateway.BaseURL, "bps_metrics_url": gateway.MetricsURL, "model": *model, "data_dir": *dir, "resources": map[string]any{"users": userIDs, "groups": groupIDs, "accounts": accountIDs}}
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
