using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/** Request body for one-click Cloudflare Worker deployment. */
model WorkerDeployRequest {
  /**
   * Cloudflare API token with Workers Scripts:Edit, Workers Routes:Edit,
   * Zone:Read, DNS:Edit, and Account:Read permissions.  Sent once and
   * encrypted server-side; subsequent reads return no value.
   */
  cloudflareApiToken: string;

  /**
   * Optional DNS zone to create a `teldrive.` subdomain on (e.g. `example.com`).
   * When provided the token must also have DNS:Edit permission; the backend will
   * create an A record `teldrive.{zone}` pointing at the VPS public IP and bind
   * the Worker to that hostname.  Leave empty to skip DNS creation and bind the
   * Worker to the existing `teldrive-gateway.{zone}` hostname on the first zone.
   */
  targetZone?: string;
}

/** Successful worker deployment response. */
model WorkerDeployResponse {
  /** Fully-qualified Worker base URL (e.g. https://teldrive-gateway.example.com). */
  workerUrl: string;

  /** DNS zone name that hosts the Worker (e.g. example.com). */
  zoneName: string;

  /** Cloudflare account ID that owns the Worker. */
  accountId: string;
}

/** Write-only settings probe — never returns secrets. */
model SettingsProbe {
  /** True once a Cloudflare API token has been saved (encrypted). */
  cloudflareApiTokenConfigured: boolean;

  /** True when the gateway is currently pointed at a Worker URL. */
  gatewayEnabled: boolean;
}

/** Which data plane serves download and upload bytes. */
enum GatewayMode {
  /** Bytes are redirected to the Cloudflare Worker. */
  worker: "worker",

  /** Bytes are streamed by this server straight from Telegram. */
  vps: "vps",
}

/** Gateway data-plane settings surfaced in the settings UI. */
model GatewaySettings {
  /** True when the gateway data plane (Cloudflare Worker) is configured. */
  gatewayEnabled: boolean;

  /**
   * Downloads smaller than this many bytes stay on the origin server instead
   * of being redirected to the Cloudflare Worker: small files (thumbnails,
   * images) suffer the Worker's extra edge hop.  `0` routes every
   * gateway-eligible file through the Worker.
   */
  workerMinBytes: int64;

  /** Data plane currently serving bytes. */
  mode: GatewayMode;

  /** Configured Worker base URL, empty when the gateway was never deployed. */
  workerUrl: string;
}

/** Mutable gateway settings. */
model GatewaySettingsUpdate {
  /**
   * New download-split threshold in bytes.  Must be zero or positive.  Omit to
   * leave the current threshold unchanged.
   */
  @minValue(0)
  workerMinBytes?: int64;

  /**
   * Switch the data plane.  `worker` requires a deployed, reachable Worker;
   * the server probes `{workerUrl}/healthz` first and rejects the change when
   * the Worker does not answer, so traffic is never pointed at a dead host.
   * `vps` always succeeds and falls back to streaming through this server.
   */
  mode?: GatewayMode;
}

@tag("Settings")
@route("/v1/settings")
@useAuth(TelDriveAuth)
namespace Settings {
  /** Probe stored configuration without leaking secrets. */
  @get
  @route("/probe")
  @operationId("getSettingsProbe")
  op probe(): JsonResponse<200, SettingsProbe>;

  /**
   * One-click Cloudflare Worker deployment.  The server validates the
   * supplied API token, uploads the bundled Worker script with all
   * required secrets (enabled bot tokens + gateway HMAC secret), binds
   * it to the first DNS zone found on the account as
   * `teldrive-gateway.{zone}`, then atomically updates the gateway URL.
   *
   * Any failure along the way — bad token, missing zone, failed script
   * upload, missing bot tokens — aborts without modifying the current
   * server configuration.
   */
  @post
  @route("/worker-deploy")
  @operationId("deployWorker")
  op deployWorker(...IdempotencyHeader, @body body: WorkerDeployRequest):
    | JsonResponse<200, WorkerDeployResponse>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>;

  /** Read the gateway data-plane settings (mode, Worker URL, download split). */
  @get
  @route("/gateway")
  @operationId("getGatewaySettings")
  op getGatewaySettings(): JsonResponse<200, GatewaySettings>;

  /**
   * Hot-update the gateway data plane: switch between the Cloudflare Worker and
   * this server, and/or change the download-split threshold.  Both fields are
   * optional, so a mode switch does not require resending the threshold.
   * Persists to config.toml and applies to the running gateway service
   * immediately, without a restart.
   */
  @put
  @route("/gateway")
  @operationId("updateGatewaySettings")
  op updateGatewaySettings(@body body: GatewaySettingsUpdate):
    | JsonResponse<200, GatewaySettings>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<422>
    | ErrorResponse<500>;
}
