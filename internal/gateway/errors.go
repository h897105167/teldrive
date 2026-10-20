package gateway

// 本文件定义网关的稳定错误码：与 Cloudflare Worker 的 { "error": "<code>" } 契约一一对应，
// 错误文本本身就是机器可读的代码，绝不含任何服务端内部信息。
import "errors"

var (
	// ErrTicketMalformed 表示 ticket 缺少分隔点或存在空段。
	ErrTicketMalformed = errors.New("ticket_malformed")
	// ErrTicketSignatureInvalid 表示 HMAC 校验不通过。
	ErrTicketSignatureInvalid = errors.New("ticket_signature_invalid")
	// ErrTicketPayloadInvalid 表示载荷无法解码或字段不合法。
	ErrTicketPayloadInvalid = errors.New("ticket_payload_invalid")
	// ErrTicketExpired 表示 ticket 已过期。
	ErrTicketExpired = errors.New("ticket_expired")
	// ErrFileNotFound 表示文件不存在，manifest 端点据此返回 404。
	ErrFileNotFound = errors.New("file_not_found")
	// ErrNotEligible 表示文件的 part 布局无法由网关数据面服务。
	ErrNotEligible = errors.New("parts_not_gateway_eligible")
)
