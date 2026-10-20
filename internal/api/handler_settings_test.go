package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/config"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/gateway"
)

const gatewayTestConfig = `
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
  enabled = true
  public-url = "%s"
  secret = "c2VjcmV0"
  bot-slots = ["111"]
  worker-min-bytes = 0
`

// stubGatewayStore satisfies gateway.Store so the service counts as configured;
// the settings endpoint never reads files or parts.
type stubGatewayStore struct{}

func (stubGatewayStore) GetFileForUser(context.Context, uuid.UUID, int64) (sqlcgen.File, error) {
	return sqlcgen.File{}, nil
}

func (stubGatewayStore) ListFileParts(context.Context, uuid.UUID) ([]sqlcgen.FilePart, error) {
	return nil, nil
}

// newGatewayHandler builds a Handler wired to a real config file and a real
// gateway service, which is what the settings endpoint writes through.
func newGatewayHandler(t *testing.T, publicURL string) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := strings.Replace(gatewayTestConfig, "%s", publicURL, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	service := gateway.New(stubGatewayStore{}, gateway.Config{
		Enabled:        cfg.Gateway.Enabled,
		PublicURL:      cfg.Gateway.PublicURL,
		Secret:         cfg.Gateway.Secret,
		TicketTTL:      cfg.Gateway.TicketTTL,
		BotSlots:       cfg.Gateway.BotSlots,
		WorkerMinBytes: cfg.Gateway.WorkerMinBytes,
	})
	handler := &Handler{
		Gateway:       service,
		ConfigPath:    path,
		RuntimeConfig: &cfg,
	}
	return handler, path
}

func problemCode(t *testing.T, err error) string {
	t.Helper()
	var problem *Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error = %T (%v), want *Problem", err, err)
	}
	return problem.Code
}

func TestUpdateGatewaySettingsSwitchesToVPS(t *testing.T) {
	t.Parallel()

	handler, path := newGatewayHandler(t, "https://gw.example.workers.dev")
	ctx := context.Background()

	res, err := handler.UpdateGatewaySettings(ctx, &gen.GatewaySettingsUpdate{
		Mode: gen.NewOptGatewayMode(gen.GatewayModeVps),
	})
	if err != nil {
		t.Fatalf("UpdateGatewaySettings() error = %v", err)
	}
	settings, ok := res.(*gen.GatewaySettings)
	if !ok {
		t.Fatalf("result = %T, want *gen.GatewaySettings", res)
	}
	if settings.Mode != gen.GatewayModeVps {
		t.Fatalf("mode = %q, want vps", settings.Mode)
	}
	if !settings.GatewayEnabled {
		t.Fatal("gatewayEnabled should stay true: the Worker is still configured, just not used")
	}
	if handler.Gateway.Enabled() {
		t.Fatal("gateway still enabled after switching to the VPS data plane")
	}
	if handler.RuntimeConfig.Gateway.Enabled {
		t.Fatal("runtime config was not updated alongside the live switch")
	}

	// The switch must survive a restart, so the file is the assertion that matters.
	reloaded, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Gateway.Enabled {
		t.Fatal("gateway.enabled was not persisted as false")
	}
}

func TestUpdateGatewaySettingsRefusesUnreachableWorker(t *testing.T) {
	t.Parallel()

	// Port 1 on loopback refuses instantly, so the health probe fails fast
	// instead of waiting out the timeout.
	handler, path := newGatewayHandler(t, "http://127.0.0.1:1")

	_, err := handler.UpdateGatewaySettings(context.Background(), &gen.GatewaySettingsUpdate{
		Mode: gen.NewOptGatewayMode(gen.GatewayModeWorker),
	})
	if got := problemCode(t, err); got != "gateway_unreachable" {
		t.Fatalf("code = %q, want gateway_unreachable", got)
	}
	if !handler.Gateway.Enabled() {
		t.Fatal("the rejected switch must leave the previous mode in place")
	}
	reloaded, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !reloaded.Gateway.Enabled {
		t.Fatal("a rejected switch must not touch config.toml")
	}
}

func TestUpdateGatewaySettingsAcceptsLiveWorker(t *testing.T) {
	t.Parallel()

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(worker.Close)

	handler, _ := newGatewayHandler(t, worker.URL)

	// Start from the VPS plane so the switch to worker is a real transition.
	if _, err := handler.UpdateGatewaySettings(context.Background(), &gen.GatewaySettingsUpdate{
		Mode: gen.NewOptGatewayMode(gen.GatewayModeVps),
	}); err != nil {
		t.Fatalf("switch to vps: %v", err)
	}
	if handler.Gateway.Enabled() {
		t.Fatal("precondition: gateway still enabled after switching to vps")
	}

	res, err := handler.UpdateGatewaySettings(context.Background(), &gen.GatewaySettingsUpdate{
		Mode: gen.NewOptGatewayMode(gen.GatewayModeWorker),
	})
	if err != nil {
		t.Fatalf("switch to worker: %v", err)
	}
	settings, ok := res.(*gen.GatewaySettings)
	if !ok {
		t.Fatalf("result = %T, want *gen.GatewaySettings", res)
	}
	if settings.Mode != gen.GatewayModeWorker {
		t.Fatalf("mode = %q, want worker", settings.Mode)
	}
	if !handler.Gateway.Enabled() {
		t.Fatal("gateway not enabled after a successful switch to worker mode")
	}
	if got := settings.WorkerUrl; got != worker.URL {
		t.Fatalf("workerUrl = %q, want %q", got, worker.URL)
	}
}

func TestUpdateGatewaySettingsRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	handler, _ := newGatewayHandler(t, "https://gw.example.workers.dev")
	ctx := context.Background()

	if _, err := handler.UpdateGatewaySettings(ctx, nil); problemCode(t, err) != "invalid_request" {
		t.Fatalf("nil body code = %q", problemCode(t, err))
	}
	if _, err := handler.UpdateGatewaySettings(ctx, &gen.GatewaySettingsUpdate{}); problemCode(t, err) != "invalid_request" {
		t.Fatalf("empty update code = %q", problemCode(t, err))
	}
	if _, err := handler.UpdateGatewaySettings(ctx, &gen.GatewaySettingsUpdate{
		WorkerMinBytes: gen.NewOptInt64(-1),
	}); problemCode(t, err) != "invalid_request" {
		t.Fatalf("negative threshold code = %q", problemCode(t, err))
	}
}

func TestUpdateGatewaySettingsRejectsWorkerWhenNotConfigured(t *testing.T) {
	t.Parallel()

	// No public URL ⇒ Configured() is false, so the Worker branch is refused
	// before any network probe.
	// A missing public URL keeps Configured() false even with a store attached,
	// so the Worker branch is refused before any network probe.
	handler := &Handler{
		Gateway: gateway.New(stubGatewayStore{}, gateway.Config{
			Secret:    "s",
			BotSlots:  []string{"111"},
			TicketTTL: time.Minute,
		}),
		ConfigPath:    filepath.Join(t.TempDir(), "config.toml"),
		RuntimeConfig: &config.Config{},
	}
	_, err := handler.UpdateGatewaySettings(context.Background(), &gen.GatewaySettingsUpdate{
		Mode: gen.NewOptGatewayMode(gen.GatewayModeWorker),
	})
	if got := problemCode(t, err); got != "gateway_not_configured" {
		t.Fatalf("code = %q, want gateway_not_configured", got)
	}
}

func TestGetGatewaySettingsReportsLiveMode(t *testing.T) {
	t.Parallel()

	handler, _ := newGatewayHandler(t, "https://gw.example.workers.dev")
	settings, err := handler.GetGatewaySettings(context.Background())
	if err != nil {
		t.Fatalf("GetGatewaySettings() error = %v", err)
	}
	if settings.Mode != gen.GatewayModeWorker {
		t.Fatalf("mode = %q, want worker", settings.Mode)
	}
	if !settings.GatewayEnabled {
		t.Fatal("gatewayEnabled = false for a configured gateway")
	}

	handler.Gateway.SetEnabled(false)
	settings, err = handler.GetGatewaySettings(context.Background())
	if err != nil {
		t.Fatalf("GetGatewaySettings() after switch error = %v", err)
	}
	if settings.Mode != gen.GatewayModeVps {
		t.Fatalf("mode = %q after hot switch, want vps", settings.Mode)
	}
}
