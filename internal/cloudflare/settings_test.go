package cloudflare

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tgdrive/teldrive/v2/internal/config"
)

// testConfigTOML is the smallest config the loader accepts: Validate() demands a
// database URL, Telegram credentials, and both security keys.
const testConfigTOML = `
[http]
  address = "127.0.0.1:43829"

[database]
  url = "postgres://example/teldrive"

[telegram]
  app-id = 12345
  app-hash = "telegram-app-hash"

[security]
  signing-key = "0123456789abcdef0123456789abcdef"
  data-key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

[gateway]
  enabled = false
  public-url = "https://gateway.example.workers.dev"
  secret = "c2VjcmV0"
  bot-slots = ["111", "222"]
  worker-min-bytes = 1048576
`

func writeTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(testConfigTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadTestConfig(t *testing.T, path string) config.Config {
	t.Helper()
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestSaveGatewaySettingsPersistsModeAndThreshold(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t)
	enabled := true
	threshold := int64(0)
	if err := SaveGatewaySettings(path, &enabled, &threshold); err != nil {
		t.Fatalf("SaveGatewaySettings() error = %v", err)
	}

	cfg := loadTestConfig(t, path)
	if !cfg.Gateway.Enabled {
		t.Fatal("gateway.enabled was not persisted")
	}
	if cfg.Gateway.WorkerMinBytes != 0 {
		t.Fatalf("gateway.worker-min-bytes = %d, want 0", cfg.Gateway.WorkerMinBytes)
	}
	// Untouched fields must survive the rewrite.
	if cfg.Gateway.PublicURL != "https://gateway.example.workers.dev" {
		t.Fatalf("public-url changed to %q", cfg.Gateway.PublicURL)
	}
	if len(cfg.Gateway.BotSlots) != 2 || cfg.Gateway.BotSlots[0] != "111" {
		t.Fatalf("bot-slots changed to %#v", cfg.Gateway.BotSlots)
	}
	if cfg.Security.DataKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("security.data-key changed to %q", cfg.Security.DataKey)
	}
}

func TestSaveGatewaySettingsLeavesOmittedFieldsAlone(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t)
	threshold := int64(16777216)
	if err := SaveGatewaySettings(path, nil, &threshold); err != nil {
		t.Fatalf("SaveGatewaySettings() error = %v", err)
	}

	cfg := loadTestConfig(t, path)
	if cfg.Gateway.Enabled {
		t.Fatal("gateway.enabled should have stayed false when only the threshold was updated")
	}
	if cfg.Gateway.WorkerMinBytes != 16777216 {
		t.Fatalf("gateway.worker-min-bytes = %d, want 16777216", cfg.Gateway.WorkerMinBytes)
	}
}

func TestSaveGatewaySettingsWritesBackupAndKeepsOriginalOnBadInput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("not a config"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	enabled := true
	if err := SaveGatewaySettings(path, &enabled, nil); err == nil {
		t.Fatal("SaveGatewaySettings() on an unparsable config = nil, want error")
	}

	// The failed write must not clobber the file the operator still has.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after failed save: %v", err)
	}
	if string(raw) != "not a config" {
		t.Fatalf("config was modified by a failed save: %q", raw)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	backups := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "config.toml.bak-") {
			backups++
		}
	}
	if backups != 1 {
		t.Fatalf("backup files = %d, want 1", backups)
	}
}

func TestSaveGatewaySettingsRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	enabled := true
	if err := SaveGatewaySettings("  ", &enabled, nil); err == nil {
		t.Fatal("SaveGatewaySettings() with an empty path = nil, want error")
	}
}

// TestSaveGatewaySettingsPrunesOldBackups guards /etc/teldrive from filling up
// with one backup per mode switch.
func TestSaveGatewaySettingsPrunesOldBackups(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t)
	dir := filepath.Dir(path)
	// Pre-seed more backups than the retention limit, with names that sort
	// oldest-first the way real timestamps do.
	const seeded = 15
	for i := 0; i < seeded; i++ {
		name := filepath.Join(dir, "config.toml.bak-202601010000"+twoDigits(i))
		if err := os.WriteFile(name, []byte("old"), 0o600); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
	}

	enabled := false
	if err := SaveGatewaySettings(path, &enabled, nil); err != nil {
		t.Fatalf("SaveGatewaySettings() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "config.toml.bak-") {
			names = append(names, entry.Name())
		}
	}
	if len(names) > configBackupKeep {
		t.Fatalf("backups = %d, want at most %d", len(names), configBackupKeep)
	}
	// The oldest seeds must be the ones dropped.
	for _, name := range names {
		if strings.HasSuffix(name, "00") || strings.HasSuffix(name, "05") {
			t.Fatalf("old backup %q survived pruning: %v", name, names)
		}
	}
}

func twoDigits(v int) string {
	if v < 10 {
		return "0" + string(rune('0'+v))
	}
	return string(rune('0'+v/10)) + string(rune('0'+v%10))
}
