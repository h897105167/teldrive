package cloudflare

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

const (
	apiBase      = "https://api.cloudflare.com/client/v4"
	workerScript = "teldrive-gateway"
	mainModule   = "index.js"
	compatDate   = "2025-08-01"
)

// Client talks to the Cloudflare REST API with a single bearer token.
type Client struct {
	token   string
	http    *http.Client
	baseURL string
}

func NewClient(token string) *Client {
	return &Client{
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: apiBase,
	}
}

// AccountInfo returned by the accounts list endpoint.
type AccountInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ZoneAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Zone represents a DNS zone on the account.
type Zone struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Account ZoneAccount `json:"account"`
}

// Result wraps every Cloudflare API response's payload envelope.
type Result struct {
	Success bool          `json:"success"`
	Errors  []ErrorItem   `json:"errors,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type ErrorItem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// plainTextBinding is a Worker metadata binding for static env vars.
// Cloudflare's API (2025–26) requires a `text` field on ALL binding types
// including plain_text — not the older `value` field.
type plainTextBinding struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// secretTextBinding mirrors Cloudflare's required shape for a secret_text
// binding in the script metadata.json.  Cloudflare's API currently (2025–26)
// validates that EVERY binding has a `text` field — even secret_text bindings
// whose real value is provided via the separate secrets endpoint.  We send
// an empty string here and let SetSecret() upload the actual value.
type secretTextBinding struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// WorkerMetadata matches Cloudflare's multipart metadata.json schema.
type WorkerMetadata struct {
	MainModule        string  `json:"main_module"`
	HasModules        bool    `json:"has_modules"`
	CompatibilityDate string  `json:"compatibility_date"`
	Bindings          []any   `json:"bindings"`
}

// WorkerVars are the static plain_text env vars baked into the Worker.
type WorkerVars struct {
	ChannelID         string
	ManifestURL       string
	UploadCompleteURL string
}

// VerifyToken calls GET /accounts to confirm the token is valid and returns
// every account the token can access (with account IDs).
func (c *Client) VerifyToken() ([]AccountInfo, error) {
	body, err := c.do("GET", "/accounts", nil)
	if err != nil {
		return nil, fmt.Errorf("verify Cloudflare API token: %w", err)
	}
	var resp Result
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse accounts response: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("Cloudflare API: %s", joinErrors(resp.Errors))
	}
	var accounts []AccountInfo
	if err := json.Unmarshal(resp.Result, &accounts); err != nil {
		return nil, fmt.Errorf("decode accounts: %w", err)
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("Cloudflare token has no accessible accounts")
	}
	return accounts, nil
}

// ListZones returns every DNS zone the token can access.  We intentionally
// hit the account-agnostic `/zones` endpoint rather than
// `/accounts/{account_id}/zones` because Cloudflare occasionally returns 400
// "No route for that URI" on the latter when the token's account_id does
// not map directly to the zone-bearing account (multi-account orgs, etc.).
func (c *Client) ListZones() ([]Zone, error) {
	body, err := c.do("GET", "/zones", nil)
	if err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	var resp Result
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse zones response: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("Cloudflare zones API: %s", joinErrors(resp.Errors))
	}
	var zones []Zone
	if err := json.Unmarshal(resp.Result, &zones); err != nil {
		return nil, fmt.Errorf("decode zones: %w", err)
	}
	return zones, nil
}

// PutWorker uploads (and overwrites) the bundled Worker script via
// multipart/form-data metadata + main module.
func (c *Client) PutWorker(accountID string, vars WorkerVars, jsBundle []byte, secretNames []string) error {
	metadata := WorkerMetadata{
		MainModule:        mainModule,
		HasModules:        true, // required for ESM module workers
		CompatibilityDate: compatDate,
		Bindings: []any{
			plainTextBinding{Type: "plain_text", Name: "CHANNEL_ID", Text: vars.ChannelID},
			plainTextBinding{Type: "plain_text", Name: "MANIFEST_URL", Text: vars.ManifestURL},
			plainTextBinding{Type: "plain_text", Name: "UPLOAD_COMPLETE_URL", Text: vars.UploadCompleteURL},
		},
	}
	for _, name := range secretNames {
		metadata.Bindings = append(metadata.Bindings, secretTextBinding{Type: "secret_text", Name: name})
	}

	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal worker metadata: %w", err)
	}

	var buf bytes.Buffer
	mp := multipart.NewWriter(&buf)

	metaPart, err := mp.CreateFormField("metadata")
	if err != nil {
		return fmt.Errorf("create metadata part: %w", err)
	}
	if _, err := metaPart.Write(metadataJSON); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	scriptHdr := make(textproto.MIMEHeader)
	scriptHdr.Set("Content-Disposition", `form-data; name="index.js"; filename="index.js"`)
	scriptHdr.Set("Content-Type", "application/javascript+module")
	scriptPart, err := mp.CreatePart(scriptHdr)
	if err != nil {
		return fmt.Errorf("create index.js part: %w", err)
	}
	if _, err := scriptPart.Write(jsBundle); err != nil {
		return fmt.Errorf("write index.js bundle: %w", err)
	}

	if err := mp.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}

	url := fmt.Sprintf("/accounts/%s/workers/scripts/%s", accountID, workerScript)
	body, err := c.doMultipart("PUT", url, mp.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("put worker script: %w", err)
	}

	var resp Result
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse put worker response: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("Cloudflare put worker: %s", joinErrors(resp.Errors))
	}
	return nil
}

// SetSecret uploads one secret to the Worker via the 2025-era Cloudflare
// Workers API (JSON body, no secret_name path segment).
func (c *Client) SetSecret(accountID, secretName, secretValue string) error {
	payload := map[string]string{
		"name": secretName,
		"text": secretValue,
		"type": "secret_text",
	}
	bodyBytes, _ := json.Marshal(payload)

	url := fmt.Sprintf("/accounts/%s/workers/scripts/%s/secrets", accountID, workerScript)
	body, err := c.do("PUT", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("set secret %q: %w", secretName, err)
	}
	var resp Result
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse set secret response: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("Cloudflare set secret %q: %s", secretName, joinErrors(resp.Errors))
	}
	return nil
}

// SetupCustomDomain attaches a Worker to a custom hostname via the 2025-era
// Cloudflare Workers API (PUT /accounts/{account_id}/workers/domains).
func (c *Client) SetupCustomDomain(accountID, hostname string) (string, error) {
	payload := map[string]string{
		"hostname": hostname,
		"service":  workerScript,
	}
	bodyBytes, _ := json.Marshal(payload)

	url := fmt.Sprintf("/accounts/%s/workers/domains", accountID)
	body, err := c.do("PUT", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("setup custom domain: %w", err)
	}
	var resp Result
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse custom domain response: %w", err)
	}
	if !resp.Success {
		return "", fmt.Errorf("Cloudflare custom-domain: %s", joinErrors(resp.Errors))
	}
	return "https://" + hostname, nil
}

// DNSRecord represents a Cloudflare DNS record (subset of fields we touch).
type DNSRecord struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Name string `json:"name"`
	Content string `json:"content"`
	TTL  int    `json:"ttl,omitempty"`
}

// CreateDNSRecord creates a DNS record on the given zone, or updates an
// existing record with the same name+type if one is already present (idempotent).
func (c *Client) CreateDNSRecord(zoneID string, rec DNSRecord) (*DNSRecord, error) {
	// 1. Check for an existing record with matching name+type.
	listURL := fmt.Sprintf("/zones/%s/dns_records?name=%s&type=%s", zoneID, rec.Name, rec.Type)
	body, err := c.do("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("list DNS records: %w", err)
	}
	var listResp struct {
		Success bool         `json:"success"`
		Result  []DNSRecord  `json:"result"`
		Errors  []ErrorItem  `json:"errors"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("parse DNS list: %w", err)
	}
	if !listResp.Success {
		return nil, fmt.Errorf("Cloudflare DNS list: %s", joinErrors(listResp.Errors))
	}

	if len(listResp.Result) > 0 {
		// Existing record — update it in place (upsert semantics).
		existing := listResp.Result[0]
		updateURL := fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, existing.ID)
		payload := map[string]any{
			"type":    rec.Type,
			"name":    rec.Name,
			"content": rec.Content,
			"ttl":     rec.TTL,
		}
		if rec.TTL == 0 {
			payload["ttl"] = 1 // auto
		}
		bodyBytes, _ := json.Marshal(payload)
		body, err = c.do("PUT", updateURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("update DNS record: %w", err)
		}
	} else {
		// 2. No match — create new record.
		url := fmt.Sprintf("/zones/%s/dns_records", zoneID)
		payload := map[string]any{
			"type":    rec.Type,
			"name":    rec.Name,
			"content": rec.Content,
		}
		if rec.TTL > 0 {
			payload["ttl"] = rec.TTL
		} else {
			payload["ttl"] = 1
		}
		bodyBytes, _ := json.Marshal(payload)
		body, err = c.do("POST", url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("create DNS record: %w", err)
		}
	}

	var resp Result
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse DNS response: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("Cloudflare DNS: %s", joinErrors(resp.Errors))
	}
	var out DNSRecord
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("decode DNS record: %w", err)
	}
	return &out, nil
}

// GetZoneByName searches the token's zones for an exact match on `name`.
// Returns nil if nothing matches; the caller decides whether that is fatal.
func (c *Client) GetZoneByName(name string) (*Zone, error) {
	zones, err := c.ListZones()
	if err != nil {
		return nil, err
	}
	for i := range zones {
		if strings.EqualFold(zones[i].Name, name) {
			return &zones[i], nil
		}
	}
	return nil, nil
}

// --- Low-level HTTP helpers -------------------------------------------------

func (c *Client) do(method, path string, body io.Reader) ([]byte, error) {
	return c.doMultipart(method, path, "application/json", body)
}

func (c *Client) doMultipart(method, path, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var errResp Result
		_ = json.Unmarshal(raw, &errResp)
		return nil, fmt.Errorf("Cloudflare HTTP %d: %s", resp.StatusCode, joinErrors(errResp.Errors))
	}
	return raw, nil
}

func joinErrors(errs []ErrorItem) string {
	if len(errs) == 0 {
		return "unknown error"
	}
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += fmt.Sprintf("%d: %s", e.Code, e.Message)
	}
	return out
}

// EncodeCiphertext base64-encodes a secureblob ciphertext so it survives
// round-trip through TOML (no binary bytes).
func EncodeCiphertext(ciphertext []byte) string {
	return base64.StdEncoding.EncodeToString(ciphertext)
}

// DecodeCiphertext is the inverse of EncodeCiphertext.
func DecodeCiphertext(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
