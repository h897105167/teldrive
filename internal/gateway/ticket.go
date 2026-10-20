package gateway

// ticket.go —— 数据面票据的签发与校验，必须与 worker/src/ticket.ts 逐字节一致。
//
// 格式： base64url(payloadJson) "." base64url(HMAC_SHA256(secret, base64url(payloadJson)))
// 签名对未解码的 payload 段字符串计算。

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ticket 版本与 scope 取值，必须与 Worker 侧一致。
const (
	// TicketVersion 是当前 ticket 结构版本，Worker 只接受该值。
	TicketVersion = 1
	// ScopeDownload 为下载票据。
	ScopeDownload = "d"
	// ScopeUpload 为上传票据（P2 阶段使用）。
	ScopeUpload = "u"
)

// Payload 是 ticket 的明文载荷，字段顺序与 JSON 键必须与 Worker 的 TicketPayload 一致，
// 因为签名覆盖序列化后的整段字符串。
type Payload struct {
	Version    int    `json:"v"`
	Scope      string `json:"s"`
	FileID     string `json:"f"`
	Range      string `json:"r,omitempty"`
	Attachment int    `json:"a,omitempty"`
	UserID     int64  `json:"u"`
	Nonce      string `json:"n"`
	ExpiresAt  int64  `json:"e"`
	// 以下字段仅上传票据（scope="u"）使用，下载票据保持零值并被 omitempty 省略，
	// 因此存量下载 ticket 的 JSON 形态完全不变，签名算法与验证逻辑向后兼容。
	// PartNo 为分块序号（1 基）；BotSlot 为 bot token 槽位（1 基）；
	// PartBytes 为该分块的明文字节数；LeaseToken 为服务端 claim 分块时签发的租约令牌，
	// Worker 回调落库时必须原样带回，防止过期租约被冒用。
	PartNo     int    `json:"p,omitempty"`
	BotSlot    int    `json:"b,omitempty"`
	PartBytes  int64  `json:"z,omitempty"`
	LeaseToken string `json:"l,omitempty"`
}

// Sign 生成 ticket：segment 为紧凑 JSON 的 base64url，签名段为其 HMAC-SHA256 的 base64url。
func Sign(secret string, payload Payload) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	segment := base64.RawURLEncoding.EncodeToString(body)
	return segment + "." + signSegment(secret, segment), nil
}

// signSegment 对 payload 段字符串本身计算 HMAC-SHA256 并做 base64url 编码。
func signSegment(secret, segment string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(segment))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 按 worker/src/ticket.ts 的 verifyTicket 逐条校验 ticket。
func Verify(secret, token string, now time.Time) (Payload, error) {
	separator := strings.IndexByte(token, '.')
	if separator <= 0 || separator == len(token)-1 {
		return Payload{}, ErrTicketMalformed
	}
	segment, signature := token[:separator], token[separator+1:]
	if !hmac.Equal([]byte(signSegment(secret, segment)), []byte(signature)) {
		return Payload{}, ErrTicketSignatureInvalid
	}

	// UseNumber 让字段以 json.Number / string / nil 原样出现，从而能逐字段复刻 Worker 的类型检查
	// （尤其是 typeof payload.e === "number"）。
	body, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return Payload{}, ErrTicketPayloadInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	fields := map[string]any{}
	if err := decoder.Decode(&fields); err != nil {
		return Payload{}, ErrTicketPayloadInvalid
	}
	if !isTicketVersion(fields["v"]) || !isDownloadOrUploadScope(fields["s"]) {
		return Payload{}, ErrTicketPayloadInvalid
	}
	fileID, ok := fields["f"].(string)
	if !ok || fileID == "" {
		return Payload{}, ErrTicketPayloadInvalid
	}
	// e 必须是 JSON 数字：字符串、布尔、null 或缺失都视为载荷非法。
	expiresNumber, ok := fields["e"].(json.Number)
	if !ok {
		return Payload{}, ErrTicketPayloadInvalid
	}
	expiresAt, err := strconv.ParseInt(expiresNumber.String(), 10, 64)
	if err != nil {
		return Payload{}, ErrTicketPayloadInvalid
	}
	if expiresAt <= now.Unix() {
		return Payload{}, ErrTicketExpired
	}
	rangeValue, _ := fields["r"].(string)
	userID, _ := optionalNumber(fields["u"])
	attachment, _ := optionalNumber(fields["a"])
	nonce, _ := fields["n"].(string)
	partNo, _ := optionalNumber(fields["p"])
	botSlot, _ := optionalNumber(fields["b"])
	partBytes, _ := optionalNumber(fields["z"])
	leaseToken, _ := fields["l"].(string)
	return Payload{
		Version: TicketVersion, Scope: fields["s"].(string), FileID: fileID, Range: rangeValue,
		Attachment: int(attachment), UserID: userID, Nonce: nonce, ExpiresAt: expiresAt,
		PartNo: int(partNo), BotSlot: int(botSlot), PartBytes: partBytes, LeaseToken: leaseToken,
	}, nil
}

func isTicketVersion(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	version, err := strconv.Atoi(number.String())
	return err == nil && version == TicketVersion
}

func isDownloadOrUploadScope(value any) bool {
	scope, ok := value.(string)
	return ok && (scope == ScopeDownload || scope == ScopeUpload)
}

// optionalNumber 读取可选的数字字段：缺失或类型不符时按 0 处理，与 Worker 的宽松读取一致。
func optionalNumber(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
