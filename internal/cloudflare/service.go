package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/config"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
)

const defaultWorkerBundlePath = "worker/index.js" // relative to data dir; see defaultBundlePath below

// DeployRequest carries everything the orchestrator needs for one-click
// deployment.  PlaintextAPIToken is only supplied on the very first deploy
// or when the user rotates credentials; otherwise the ciphertext on the
// Config is used and this field stays empty.
type DeployRequest struct {
	PlaintextAPIToken string

	// Worker static env vars are read from the gateway and gateway-manifest
	// URLs; they cannot be overridden at this layer to keep the VPS the
	// single source of truth.
	ManifestURL       string
	UploadCompleteURL string
	ChannelID         string
	GatewaySecret     string
	BotTokenSlots     []string // ordered slot list; slot n maps to BOT_TOKEN_n+1 secret

	// Resolved bot token values (decrypted from bots table).  The caller
	// must supply exactly len(BotTokenSlots) entries.
	BotTokens []string

	// TargetZone — if set to a DNS zone owned by the token, the orchestrator
	// will create an A record `teldrive.{TargetZone}` → ServerIP and bind
	// the Worker there instead of the default `teldrive-gateway.{firstZone}`.
	TargetZone string

	// ServerIP — the VPS public IP used when creating the A record above.
	// Only consulted when TargetZone is non-empty.
	ServerIP string

	// ReuseWorkerURL — when non-empty and TargetZone is empty, the
	// orchestrator keeps this already-bound Worker URL instead of re-binding a
	// custom domain.  Re-deploys that only sync secrets (e.g. newly enabled
	// bots) therefore stay idempotent and avoid Cloudflare error 100117 when
	// the hostname already carries a proxied A record / Worker Route.
	ReuseWorkerURL string
}

// DeployResult reports the outcome of a successful orchestration.
type DeployResult struct {
	WorkerURL string
	ZoneName  string
	AccountID string
	// DNSHostname is only set when a DNS record was created (TargetZone flow).
	DNSHostname string
}

// Deployer wires the Client, the secureblob cipher, and configuration
// together for a single atomic deployment.  Any step failure leaves the
// existing config.toml untouched.
type Deployer struct {
	Cipher       *secureblob.Cipher
	ConfigPath   string
	BundlePath   string // optional override; falls back to default location
}

var (
	ErrNoZone          = errors.New("Cloudflare account has no DNS zones; create one before deploying the Worker")
	ErrNoBotTokens     = errors.New("no enabled Telegram bot tokens available; add at least one bot before deploying")
	ErrGatewaySecret   = errors.New("gateway HMAC secret is empty; set [gateway] secret in config.toml first")
	ErrMissingCipher    = errors.New("secureblob cipher is not initialised")
	ErrEmptyBundle      = errors.New("Worker JS bundle is empty; rebuild with `bun build worker/src/index.ts --format esm --outfile worker/dist/index.js` and install to the VPS bundle path")
	ErrMissingTokens    = errors.New("deploy request bot token count does not match gateway.bot-slots count")
	ErrZoneNotFound     = errors.New("Cloudflare zone not found for the supplied targetZone — verify the domain name and that the token has Zone:Read permission")
	ErrMissingServerIP  = errors.New("targetZone was supplied but serverIP is empty — the backend could not determine the VPS public IP")
)

// Orchestrate validates every precondition, then calls the Cloudflare API in
// order, finally updating config.toml with the new [cloudflare] and
// [gateway] settings atomically.
func (d *Deployer) Orchestrate(ctx context.Context, req DeployRequest) (*DeployResult, error) {
	_ = ctx
	if d.Cipher == nil {
		return nil, ErrMissingCipher
	}
	if strings.TrimSpace(req.GatewaySecret) == "" {
		return nil, ErrGatewaySecret
	}
	if len(req.BotTokens) != len(req.BotTokenSlots) {
		return nil, fmt.Errorf("%w: got %d tokens, slots want %d",
			ErrMissingTokens, len(req.BotTokens), len(req.BotTokenSlots))
	}

	// 1. Load the Worker JS bundle from disk.
	bundlePath := d.BundlePath
	if bundlePath == "" {
		bundlePath = defaultBundlePath()
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("read Worker bundle at %q: %w", bundlePath, err)
	}
	if len(bundle) == 0 {
		return nil, fmt.Errorf("%w at %q", ErrEmptyBundle, bundlePath)
	}

	// 2. Token source — either the freshly-supplied plaintext or the one
	//    previously encrypted into config.toml.
	apiToken := strings.TrimSpace(req.PlaintextAPIToken)
	var tokenCiphertext []byte
	if apiToken != "" {
		enc, err := d.Cipher.Seal("cloudflare-api-token", []byte(apiToken))
		if err != nil {
			return nil, fmt.Errorf("encrypt Cloudflare API token: %w", err)
		}
		tokenCiphertext = enc
	} else {
		var cfg config.Config
		if strings.TrimSpace(d.ConfigPath) != "" {
			cfg, err = config.LoadFromPath(d.ConfigPath)
		} else {
			cfg, err = config.Load()
		}
		if err != nil {
			return nil, fmt.Errorf("load config to read Cloudflare token: %w", err)
		}
		if strings.TrimSpace(cfg.Cloudflare.APITokenCiphertext) == "" {
			return nil, fmt.Errorf("Cloudflare API token is not configured — supply one in the request body")
		}
		ct, err := DecodeCiphertext(cfg.Cloudflare.APITokenCiphertext)
		if err != nil {
			return nil, fmt.Errorf("decode stored Cloudflare ciphertext: %w", err)
		}
		plain, err := d.Cipher.Open("cloudflare-api-token", ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt stored Cloudflare API token: %w", err)
		}
		apiToken = string(plain)
	}

	client := NewClient(apiToken)

	// 3. Validate token → confirm it has access to at least one account.
	if _, err = client.VerifyToken(); err != nil {
		return nil, fmt.Errorf("validate Cloudflare token: %w", err)
	}

	// 4. Resolve the DNS zone.  If the caller supplied a targetZone we
	//    require an exact match; otherwise fall back to the first zone.
	var zone Zone
	if strings.TrimSpace(req.TargetZone) != "" {
		matched, findErr := client.GetZoneByName(strings.TrimSpace(req.TargetZone))
		if findErr != nil {
			return nil, fmt.Errorf("find target zone %q: %w", req.TargetZone, findErr)
		}
		if matched == nil {
			return nil, fmt.Errorf("%w: %s", ErrZoneNotFound, req.TargetZone)
		}
		zone = *matched
	} else {
		zones, listErr := client.ListZones()
		if listErr != nil {
			return nil, fmt.Errorf("list zones: %w", listErr)
		}
		if len(zones) == 0 {
			return nil, ErrNoZone
		}
		zone = zones[0]
	}
	accountID := zone.Account.ID

	// 5. Compute hostname.  When targetZone is supplied we skip creating an
	//    A record — Cloudflare Worker Custom Domain provisions its own DNS
	//    records (CNAME → *.workers.dev) and rejects hostnames that already
	//    carry A/CNAME records (HTTP 409, error 100117).
	var hostname string
	var dnsHostname string
	if strings.TrimSpace(req.TargetZone) != "" {
		if strings.TrimSpace(req.ServerIP) == "" {
			return nil, ErrMissingServerIP
		}
		hostname = "teldrive." + zone.Name
		dnsHostname = hostname
	} else {
		hostname = "teldrive-gateway." + zone.Name
	}

	// 6. Names for the Worker bindings.
	var secretNames []string
	secretNames = append(secretNames, "GATEWAY_SECRET")
	for i := range req.BotTokens {
		secretNames = append(secretNames, fmt.Sprintf("BOT_TOKEN_%d", i+1))
	}

	// 7. Upload the Worker script (PUT — Cloudflare overwrites same-named
	//    scripts atomically, no DELETE+CREATE window).
	if err := client.PutWorker(accountID, WorkerVars{
		ChannelID:         req.ChannelID,
		ManifestURL:       req.ManifestURL,
		UploadCompleteURL: req.UploadCompleteURL,
	}, bundle, secretNames); err != nil {
		return nil, fmt.Errorf("upload Worker script: %w", err)
	}

	// 8. Push each secret.
	if err := client.SetSecret(accountID, "GATEWAY_SECRET", req.GatewaySecret); err != nil {
		return nil, fmt.Errorf("set GATEWAY_SECRET: %w", err)
	}
	for i, tok := range req.BotTokens {
		name := fmt.Sprintf("BOT_TOKEN_%d", i+1)
		if err := client.SetSecret(accountID, name, tok); err != nil {
			return nil, fmt.Errorf("set %s: %w", name, err)
		}
	}

	// 9. Bind the Worker to the chosen hostname → workerURL.  Re-deploys that
	//    only need to sync secrets/vars keep the existing binding (see
	//    ReuseWorkerURL) instead of re-binding a custom domain.
	workerURL := strings.TrimRight(strings.TrimSpace(req.ReuseWorkerURL), "/")
	if workerURL == "" || strings.TrimSpace(req.TargetZone) != "" {
		workerURL, err = client.SetupCustomDomain(accountID, hostname)
		if err != nil {
			return nil, fmt.Errorf("bind custom domain: %w", err)
		}
	}

	// 10. Atomically update config.toml.  Only after every Cloudflare API
	//     call succeeded do we persist the new settings.
	newBundlePath := bundlePath // persist the resolved path so restarts find the bundle
	if err := d.updateConfig(workerURL, EncodeCiphertext(tokenCiphertext), newBundlePath, req.BotTokenSlots); err != nil {
		return nil, fmt.Errorf("update config.toml: %w", err)
	}

	return &DeployResult{
		WorkerURL:   workerURL,
		ZoneName:    zone.Name,
		AccountID:   accountID,
		DNSHostname: dnsHostname,
	}, nil
}

// defaultBundlePath returns the default Worker bundle location for the
// current platform.  The app checks this path on startup so operators can
// drop a fresh Worker without touching config.toml.
func defaultBundlePath() string {
	switch runtime.GOOS {
	case "linux":
		candidates := []string{
			os.Getenv("XDG_DATA_HOME"),
			"/usr/local/share/teldrive",
		}
		for _, dir := range candidates {
			if dir == "" {
				continue
			}
			p := filepath.Join(dir, "teldrive", "worker", "index.js")
			if info, err := os.Stat(p); err == nil && info.Size() > 0 {
				return p
			}
		}
		return "/usr/local/share/teldrive/worker/index.js"
	default:
		return defaultWorkerBundlePath // placeholder; non-production deployments
	}
}

// updateConfig reads the existing TOML, rewrites the [cloudflare] block,
// rewrites [gateway].public-url, and overwrites the file.
func (d *Deployer) updateConfig(newWorkerURL, newCiphertext, newBundlePath string, newBotSlots []string) error {
	raw, err := os.ReadFile(d.ConfigPath)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format("20060102150405")
	if err := os.WriteFile(d.ConfigPath+".bak-"+now, raw, 0o644); err != nil {
		return fmt.Errorf("write config backup: %w", err)
	}

	cfg, err := config.LoadFromPath(d.ConfigPath)
	if err != nil {
		return fmt.Errorf("parse current config from %q: %w", d.ConfigPath, err)
	}
	cfg.Gateway.Enabled = true
	cfg.Gateway.PublicURL = newWorkerURL
	cfg.Cloudflare.APITokenCiphertext = newCiphertext
	// Persist the slot order so the manifest builder keeps mapping part→slot
	// with the same numbering the Worker received as BOT_TOKEN_n secrets.
	if len(newBotSlots) > 0 {
		cfg.Gateway.BotSlots = append([]string(nil), newBotSlots...)
	}
	if strings.TrimSpace(newBundlePath) != "" {
		cfg.Cloudflare.BundlePath = newBundlePath
	}

	tomlBytes, err := marshalConfigTOML(&cfg)
	if err != nil {
		return err
	}

	if err := os.WriteFile(d.ConfigPath, tomlBytes, 0o644); err != nil {
		return fmt.Errorf("overwrite config: %w", err)
	}
	return nil
}
