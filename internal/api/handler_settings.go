package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/cloudflare"
	"github.com/tgdrive/teldrive/v2/internal/config"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
)

// ConfigureCloudflare wires the Cloudflare deploy onto the main Handler.
func (h *Handler) ConfigureCloudflare(cipher *secureblob.Cipher, configPath string, cfg *config.Config) *Handler {
	if h != nil {
		h.CloudflareCipher = cipher
		h.ConfigPath = configPath
		h.RuntimeConfig = cfg
	}
	return h
}

// DeployWorker implements the ogen DeployWorker operation.
func (h *Handler) DeployWorker(ctx context.Context, req *gen.WorkerDeployRequest, params gen.DeployWorkerParams) (gen.DeployWorkerRes, error) {
	_ = params
	if h.CloudflareCipher == nil {
		return nil, fmt.Errorf("Cloudflare cipher is not initialised")
	}
	if h.RuntimeConfig == nil {
		return nil, fmt.Errorf("runtime config is not available")
	}
	if h.Pool == nil {
		return nil, fmt.Errorf("database pool is not available")
	}

	// 1. Resolve every enabled bot owned by the caller (slot-ordered).  Bots
	//    already present in gateway.bot-slots keep their position so existing
	//    part→slot mappings stay valid; newly enabled bots are appended.
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve requesting user: %w", err)
	}
	botSlots, botTokens, err := h.resolveEnabledBots(ctx, userID, h.RuntimeConfig.Gateway.BotSlots)
	if err != nil {
		return nil, fmt.Errorf("resolve bot tokens: %w", err)
	}
	if len(botTokens) == 0 {
		return nil, fmt.Errorf("%w", cloudflare.ErrNoBotTokens)
	}

	// 2. If a targetZone was specified, probe the VPS public IP so we can
	//    create the A record without asking the user to copy/paste it.
	var serverIP string
	targetZone := strings.TrimSpace(req.GetTargetZone().Value)
	if targetZone != "" {
		var err error
		serverIP, err = fetchPublicIP(ctx)
		if err != nil {
			return nil, fmt.Errorf("probe VPS public IP for DNS record: %w", err)
		}
	}

	// 3. Assemble the deploy request.  Worker callbacks (manifest /
	//    upload-complete) must reach the control-plane origin, which may differ
	//    from the gateway public URL when the data plane is served by the Worker
	//    itself (origin-url override).  The Worker URL itself is reused from the
	//    gateway public URL so that re-deploys — e.g. syncing newly enabled
	//    bots — stay idempotent.
	originURL := strings.TrimRight(strings.TrimSpace(h.RuntimeConfig.Gateway.OriginURL), "/")
	if originURL == "" {
		originURL = strings.TrimRight(strings.TrimSpace(h.RuntimeConfig.Gateway.PublicURL), "/")
	}
	deployReq := cloudflare.DeployRequest{
		PlaintextAPIToken: strings.TrimSpace(req.GetCloudflareApiToken()),
		ManifestURL:       originURL + "/internal/gateway/manifest",
		UploadCompleteURL: originURL + "/internal/gateway/upload-complete",
		ChannelID:         h.channelIDFromGateway(),
		GatewaySecret:     h.RuntimeConfig.Gateway.Secret,
		BotTokenSlots:     botSlots,
		BotTokens:         botTokens,
		ReuseWorkerURL:    h.RuntimeConfig.Gateway.PublicURL,
		TargetZone:        targetZone,
		ServerIP:          serverIP,
	}

	deployer := &cloudflare.Deployer{
		Cipher:     h.CloudflareCipher,
		ConfigPath: h.ConfigPath,
		BundlePath: h.RuntimeConfig.Cloudflare.BundlePath,
	}

	result, err := deployer.Orchestrate(ctx, deployReq)
	if err != nil {
		return nil, fmt.Errorf("Cloudflare deploy: %w", err)
	}

	return &gen.WorkerDeployResponse{
		WorkerUrl: result.WorkerURL,
		ZoneName:  result.ZoneName,
		AccountId: result.AccountID,
	}, nil
}

// GetSettingsProbe implements the ogen GetSettingsProbe operation.
func (h *Handler) GetSettingsProbe(ctx context.Context) (*gen.SettingsProbe, error) {
	_ = ctx
	apiTokenConfigured := false
	gatewayEnabled := false
	if h.RuntimeConfig != nil {
		apiTokenConfigured = strings.TrimSpace(h.RuntimeConfig.Cloudflare.APITokenCiphertext) != ""
	}
	// Report the live switch, not the startup snapshot: the settings page shows
	// this next to the mode toggle, so it must follow a hot switch.
	gatewayEnabled = h.Gateway != nil && h.Gateway.Enabled()
	return &gen.SettingsProbe{
		CloudflareApiTokenConfigured: apiTokenConfigured,
		GatewayEnabled:               gatewayEnabled,
	}, nil
}

// GetGatewaySettings implements the ogen GetGatewaySettings operation.
func (h *Handler) GetGatewaySettings(ctx context.Context) (*gen.GatewaySettings, error) {
	_ = ctx
	return h.gatewaySettings(), nil
}

// gatewaySettings assembles the current data-plane view for both settings ops.
func (h *Handler) gatewaySettings() *gen.GatewaySettings {
	settings := &gen.GatewaySettings{Mode: gen.GatewayModeVps}
	if h.Gateway != nil {
		settings.WorkerMinBytes = h.Gateway.WorkerMinBytes()
		settings.WorkerUrl = h.Gateway.PublicURL()
		if h.Gateway.Enabled() {
			settings.Mode = gen.GatewayModeWorker
		}
		settings.GatewayEnabled = h.Gateway.Configured()
	} else if h.RuntimeConfig != nil {
		settings.WorkerMinBytes = h.RuntimeConfig.Gateway.WorkerMinBytes
		settings.WorkerUrl = h.RuntimeConfig.Gateway.PublicURL
	}
	return settings
}

// UpdateGatewaySettings implements the ogen UpdateGatewaySettings operation:
// hot-apply the data-plane mode and/or the download-split threshold, then
// persist both to config.toml.  Every field is optional so the UI can flip the
// mode without resending the threshold.
func (h *Handler) UpdateGatewaySettings(ctx context.Context, req *gen.GatewaySettingsUpdate) (gen.UpdateGatewaySettingsRes, error) {
	if req == nil {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_request", "request body is required", nil)
	}
	if h.RuntimeConfig == nil || h.ConfigPath == "" {
		return nil, problem(http.StatusInternalServerError, "config_unavailable", "runtime config is not available", nil)
	}
	if h.Gateway == nil {
		return nil, problem(http.StatusInternalServerError, "gateway_unavailable", "gateway service is not initialised", nil)
	}

	var (
		nextEnabled *bool
		nextMin     *int64
	)
	if req.WorkerMinBytes.Set {
		value := req.WorkerMinBytes.Value
		if value < 0 {
			return nil, problem(http.StatusUnprocessableEntity, "invalid_request", "worker-min-bytes must be zero or positive", nil)
		}
		nextMin = &value
	}
	if req.Mode.Set {
		switch req.Mode.Value {
		case gen.GatewayModeWorker:
			// Refuse to point traffic at a Worker that does not answer: a dead
			// host turns every download into a redirect the client cannot follow.
			if !h.Gateway.Configured() {
				return nil, problem(http.StatusUnprocessableEntity, "gateway_not_configured",
					"gateway is not configured; deploy the Worker first", nil)
			}
			if err := probeWorkerHealth(ctx, h.Gateway.PublicURL()); err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "gateway_unreachable",
					"the Cloudflare Worker did not answer its health check; staying on the VPS data plane", err)
			}
			enabled := true
			nextEnabled = &enabled
		case gen.GatewayModeVps:
			enabled := false
			nextEnabled = &enabled
		default:
			return nil, problem(http.StatusUnprocessableEntity, "invalid_request", "mode must be worker or vps", nil)
		}
	}
	if nextEnabled == nil && nextMin == nil {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_request", "nothing to update", nil)
	}

	// 1. Persist first: a failed write must not leave the process serving a mode
	//    that a restart would silently revert.
	if err := cloudflare.SaveGatewaySettings(h.ConfigPath, nextEnabled, nextMin); err != nil {
		return nil, problem(http.StatusInternalServerError, "config_write_failed", "could not persist gateway settings", err)
	}

	// 2. Hot-apply to the running service (atomic, no restart).
	if nextEnabled != nil {
		h.Gateway.SetEnabled(*nextEnabled)
		h.RuntimeConfig.Gateway.Enabled = *nextEnabled
	}
	if nextMin != nil {
		h.Gateway.SetWorkerMinBytes(*nextMin)
		h.RuntimeConfig.Gateway.WorkerMinBytes = *nextMin
	}

	return h.gatewaySettings(), nil
}

// probeWorkerHealth asks the Worker for /healthz.  It is deliberately short: the
// settings call should fail fast rather than hang the UI while a deleted Worker
// times out at the edge.
func probeWorkerHealth(ctx context.Context, publicURL string) error {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" {
		return fmt.Errorf("gateway public URL is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// resolveEnabledBots returns the slot-ordered bot IDs and their decrypted
// tokens for every enabled bot owned by userID.  Slots already present in
// existingSlots keep their original position (so part→slot mappings written by
// earlier uploads stay valid); newly enabled bots are appended in a stable
// (bot_id ascending) order.  Bots that no longer exist or are disabled are
// dropped — their old parts fall back to the VPS direct-download path.
func (h *Handler) resolveEnabledBots(ctx context.Context, userID int64, existingSlots []string) ([]string, []string, error) {
	rows, err := h.Pool.Query(ctx,
		"SELECT bot_id::text, token_ciphertext FROM teldrive.bots WHERE user_id = $1 AND enabled ORDER BY bot_id",
		userID)
	if err != nil {
		return nil, nil, fmt.Errorf("query enabled bots: %w", err)
	}
	defer rows.Close()

	tokens := make(map[string]string)
	order := make([]string, 0)
	for rows.Next() {
		var botID string
		var ciphertext []byte
		if err := rows.Scan(&botID, &ciphertext); err != nil {
			return nil, nil, fmt.Errorf("scan bot row: %w", err)
		}
		if len(ciphertext) == 0 {
			continue
		}
		plain, err := h.CloudflareCipher.Open("bot-token", ciphertext)
		if err != nil {
			continue
		}
		tokens[botID] = string(plain)
		order = append(order, botID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate bot rows: %w", err)
	}

	enabled := make(map[string]struct{}, len(order))
	for _, id := range order {
		enabled[id] = struct{}{}
	}

	// Keep existing slot order for bots that are still enabled, then append
	// any enabled bot that isn't already slotted.
	slots := make([]string, 0, len(order))
	seen := make(map[string]struct{}, len(order))
	for _, raw := range existingSlots {
		id := strings.TrimSpace(raw)
		if _, ok := enabled[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		slots = append(slots, id)
	}
	for _, id := range order {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		slots = append(slots, id)
	}

	outTokens := make([]string, 0, len(slots))
	for _, id := range slots {
		outTokens = append(outTokens, tokens[id])
	}
	return slots, outTokens, nil
}

// channelIDFromGateway returns the Telegram storage channel ID that the
// Worker must read file parts from.  Right now the Worker receives this as
// a plain_text environment variable.  We hard-code the production channel
// because the gateway service doesn't expose a runtime channel getter yet.
func (h *Handler) channelIDFromGateway() string {
	return "-1003732028072"
}

// fetchPublicIP asks a well-known external service for the VPS's own public
// IPv4 address.  We try two providers in sequence so the failure of one
// (rate-limit, outage, network policy) does not block deployment.
func fetchPublicIP(ctx context.Context) (string, error) {
	endpoints := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
	}
	client := &http.Client{Timeout: 8 * time.Second}
	for _, url := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		ip := strings.TrimSpace(string(body))
		// Light sanity check — not exhaustive (IPv6 not supported), but good
		// enough to reject HTML error pages etc.
		if strings.Count(ip, ".") == 3 {
			return ip, nil
		}
	}
	return "", fmt.Errorf("all public-IP providers failed; check the VPS egress firewall or supply server IP another way")
}
