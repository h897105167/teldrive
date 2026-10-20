package gateway

import (
	"testing"
	"time"
)

// TestSetEnabledSwitchesTheDataPlaneHot covers the settings-page switch: the
// startup Config snapshot is immutable, so a hot switch has to land in the
// atomic flag that Enabled() reads.
func TestSetEnabledSwitchesTheDataPlaneHot(t *testing.T) {
	t.Parallel()

	service := New(&fakeStore{}, Config{
		Enabled:   false,
		PublicURL: "https://gw.workers.dev/",
		Secret:    "shared-secret",
		TicketTTL: 10 * time.Minute,
		BotSlots:  testSlots,
	})
	if service.Enabled() {
		t.Fatal("Enabled() = true before SetEnabled(true)")
	}

	service.SetEnabled(true)
	if !service.Enabled() {
		t.Fatal("Enabled() = false after SetEnabled(true)")
	}

	service.SetEnabled(false)
	if service.Enabled() {
		t.Fatal("Enabled() = true after SetEnabled(false)")
	}
}

// TestConfiguredIsIndependentOfTheSwitch guards the internal endpoint
// registration: switching to the VPS data plane must not tear down the manifest
// callback the Worker needs once traffic is switched back.
func TestConfiguredIsIndependentOfTheSwitch(t *testing.T) {
	t.Parallel()

	service := New(&fakeStore{}, Config{
		Enabled:   false,
		PublicURL: "https://gw.workers.dev/",
		Secret:    "shared-secret",
		TicketTTL: 10 * time.Minute,
		BotSlots:  testSlots,
	})
	if !service.Configured() {
		t.Fatal("Configured() = false for a fully configured gateway")
	}
	if service.Enabled() {
		t.Fatal("Enabled() = true for a gateway that is configured but switched off")
	}
}

func TestConfiguredRequiresEveryPiece(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
	}{
		{"no public url", Config{Secret: "s", BotSlots: testSlots}},
		{"no secret", Config{PublicURL: "https://gw.workers.dev/", BotSlots: testSlots}},
		{"no bot slots", Config{PublicURL: "https://gw.workers.dev/", Secret: "s"}},
	}
	for _, testCase := range cases {
		service := New(&fakeStore{}, testCase.cfg)
		if service.Configured() {
			t.Fatalf("%s: Configured() = true, want false", testCase.name)
		}
		if service.Enabled() {
			t.Fatalf("%s: Enabled() = true, want false", testCase.name)
		}
	}

	var nilService *Service
	if nilService.Configured() || nilService.Enabled() {
		t.Fatal("nil service must report neither configured nor enabled")
	}
}

func TestPublicURLTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	service := New(&fakeStore{}, Config{
		Enabled:   true,
		PublicURL: "https://gw.workers.dev/",
		Secret:    "shared-secret",
		TicketTTL: 10 * time.Minute,
		BotSlots:  testSlots,
	})
	if got := service.PublicURL(); got != "https://gw.workers.dev" {
		t.Fatalf("PublicURL() = %q", got)
	}
}
