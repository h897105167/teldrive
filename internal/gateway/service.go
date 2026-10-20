package gateway

// service.go —— 控制面侧的网关服务：签发下载 ticket（供 302 到 Worker）并输出 manifest。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

// Store 抽象网关所需的最小读取能力，生产实现为 PostgreSQL 查询，测试可注入假实现。
type Store interface {
	GetFileForUser(ctx context.Context, fileID uuid.UUID, userID int64) (sqlcgen.File, error)
	ListFileParts(ctx context.Context, fileID uuid.UUID) ([]sqlcgen.FilePart, error)
}

// PoolStore 是基于 sqlcgen 的 Store 生产实现。
type PoolStore struct {
	queries *sqlcgen.Queries
}

func NewPoolStore(pool *pgxpool.Pool) *PoolStore {
	return &PoolStore{queries: sqlcgen.New(pool)}
}

func (s *PoolStore) GetFileForUser(ctx context.Context, fileID uuid.UUID, userID int64) (sqlcgen.File, error) {
	file, err := s.queries.GetFileForUser(ctx, sqlcgen.GetFileForUserParams{
		FileID: dbtypes.UUID(fileID), UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.File{}, ErrFileNotFound
		}
		return sqlcgen.File{}, err
	}
	if file == nil {
		return sqlcgen.File{}, ErrFileNotFound
	}
	return *file, nil
}

func (s *PoolStore) ListFileParts(ctx context.Context, fileID uuid.UUID) ([]sqlcgen.FilePart, error) {
	rows, err := s.queries.ListFileParts(ctx, dbtypes.UUID(fileID))
	if err != nil {
		return nil, err
	}
	parts := make([]sqlcgen.FilePart, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		parts = append(parts, *row)
	}
	return parts, nil
}

// Config 是网关运行配置，来源于 config.Gateway。
type Config struct {
	Enabled   bool
	PublicURL string
	Secret    string
	TicketTTL time.Duration
	BotSlots  []string
	// WorkerMinBytes 把小于该字节数的下载留在 VPS 直连路径：小文件经过
	// Worker 多一跳边缘往返反而更慢（图片/缩略图加载明显），大文件才值得
	// 卸载到 Worker 省下 VPS 带宽。0 表示不做大小分流，全部走 Worker。
	WorkerMinBytes int64
}

// Service 负责 302 票据签发（控制面）与 manifest 输出（数据面自证）。
type Service struct {
	store  Store
	config Config
	// enabled 是数据面开关的运行时镜像：设置端点可以在不重启的前提下把
	// 下载/上传切回 VPS 直连（例如 Worker 被停用或不可达时）。配置是否
	// 齐全由 Configured 单独判断，两者相与才是「真的走 Worker」。
	enabled atomic.Bool
	// workerMinBytes 是下载分流阈值的运行时镜像：设置端点会热更新该值，
	// 下载路径高频并发读取，因此用原子量而非直接读 config。
	workerMinBytes atomic.Int64
	now            func() time.Time
}

func New(store Store, cfg Config) *Service {
	s := &Service{store: store, config: cfg, now: time.Now}
	s.enabled.Store(cfg.Enabled)
	s.workerMinBytes.Store(cfg.WorkerMinBytes)
	return s
}

// SetEnabled 热切换数据面：true 走 Cloudflare Worker，false 全部回到本机
// 直连 Telegram。立即生效、无需重启；持久化由调用方负责。
func (s *Service) SetEnabled(v bool) {
	if s == nil {
		return
	}
	s.enabled.Store(v)
}

// Configured 报告网关是否具备运行所需的配置（与开关无关）。内部 manifest /
// upload-complete 端点按它注册：开关只影响流量走向，不能让 Worker 的回源
// 回调随开关一起消失，否则切回 Worker 模式后所有下载都会 404。
func (s *Service) Configured() bool {
	return s != nil && s.store != nil &&
		s.config.PublicURL != "" && s.config.Secret != "" && len(s.config.BotSlots) > 0
}

// Enabled 报告当前是否真的把流量卸载到 Worker：开关打开且配置齐全。
func (s *Service) Enabled() bool {
	return s != nil && s.enabled.Load() && s.Configured()
}

// PublicURL 返回 Worker 基地址，供设置页展示与连通性探测。末尾斜杠会被去掉：
// 拼接 /healthz 时出现 "//healthz" 会被边缘当成另一个路径。
func (s *Service) PublicURL() string {
	if s == nil {
		return ""
	}
	return strings.TrimRight(s.config.PublicURL, "/")
}

// SetWorkerMinBytes 热更新下载分流阈值（UI 设置端点调用），立即生效、无需重启。
func (s *Service) SetWorkerMinBytes(v int64) {
	if s == nil {
		return
	}
	s.workerMinBytes.Store(v)
}

// WorkerMinBytes 返回当前生效的下载分流阈值。
func (s *Service) WorkerMinBytes() int64 {
	if s == nil {
		return 0
	}
	return s.workerMinBytes.Load()
}

// ProxyDownload 在文件完全可由网关服务时签发下载 ticket，返回应 302 到的 Location。
// 未启用、文件不具备网关条件或任何内部错误都返回 ok=false，让调用方安全回退到 VPS 直连路径。
// 日志绝不记录 ticket、secret 或 Telegram file_path。
func (s *Service) ProxyDownload(ctx context.Context, ownerID int64, fileID uuid.UUID, file *sqlcgen.File, rng string, attachment bool) (string, bool) {
	if !s.Enabled() {
		return "", false
	}
	// 大小分流：小于阈值的文件回退 VPS 直连，避免小文件被 Worker 的额外一跳拖慢。
	// 调用方（streamFile）已保证 Size 有效且非负。
	if min := s.workerMinBytes.Load(); min > 0 && file.Size.Int64 < min {
		return "", false
	}
	parts, err := s.store.ListFileParts(ctx, fileID)
	if err != nil {
		slog.ErrorContext(ctx, "gateway.parts_lookup_failed", "file_id", fileID, "error", err)
		return "", false
	}
	if !Eligible(file, parts, s.config.BotSlots) {
		return "", false
	}
	nonce, err := randomNonce()
	if err != nil {
		slog.ErrorContext(ctx, "gateway.nonce_failed", "file_id", fileID, "error", err)
		return "", false
	}
	payload := Payload{
		Version: TicketVersion, Scope: ScopeDownload, FileID: fileID.String(),
		Range: strings.TrimSpace(rng), UserID: ownerID, Nonce: nonce,
		ExpiresAt: s.now().Add(s.config.TicketTTL).Unix(),
	}
	if attachment {
		payload.Attachment = 1
	}
	ticket, err := Sign(s.config.Secret, payload)
	if err != nil {
		slog.ErrorContext(ctx, "gateway.ticket_sign_failed", "file_id", fileID, "error", err)
		return "", false
	}
	return strings.TrimRight(strings.TrimSpace(s.config.PublicURL), "/") + "/d/" + ticket, true
}

// LoadManifest 组装指定文件的网关清单；文件不存在时返回 ErrFileNotFound。
func (s *Service) LoadManifest(ctx context.Context, fileID uuid.UUID, ownerID int64) (*Manifest, error) {
	file, err := s.store.GetFileForUser(ctx, fileID, ownerID)
	if err != nil {
		return nil, err
	}
	parts, err := s.store.ListFileParts(ctx, fileID)
	if err != nil {
		return nil, err
	}
	return BuildManifest(&file, parts, s.config.BotSlots, contentETag(&file))
}

// UploadEligible 判断上传会话是否允许走网关数据面：未加密、未声明期望哈希
// （服务端无法为 Worker 直传的字节验哈希）、分块尺寸恰为网关约定的 16 MiB。
func UploadEligible(session *sqlcgen.UploadSession) bool {
	return session != nil && !session.Encryption &&
		!session.ExpectedHashAlgorithm.Valid && session.PartSize == PartSize
}

// TicketTTL 返回票据有效期，供调用方同步设置分块租约时长。
func (s *Service) TicketTTL() time.Duration {
	return s.config.TicketTTL
}

// SelectBotSlot 随机选择一个 bot 槽位，返回其 Telegram 数字 ID 与 1 基槽位号。
// 配置中出现无法解析的槽位（属启动期配置错误）时返回 ok=false。
func (s *Service) SelectBotSlot() (int64, int, bool) {
	if !s.Enabled() {
		return 0, 0, false
	}
	count := len(s.config.BotSlots)
	index, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		slog.ErrorContext(context.Background(), "gateway.bot_slot_failed", "error", err)
		return 0, 0, false
	}
	slot := int(index.Int64()) + 1
	botID, err := strconv.ParseInt(strings.TrimSpace(s.config.BotSlots[slot-1]), 10, 64)
	if err != nil || botID <= 0 {
		slog.Error("gateway.bot_slot_invalid", "slot", slot)
		return 0, 0, false
	}
	return botID, slot, true
}

// UploadURL 为已 claim 的 bot 分块签发上传 ticket，返回客户端应 PUT 到的 Worker URL。
// leaseToken 必须来自 ClaimPartBot 的签发结果，Worker 回调落库时原样带回。
func (s *Service) UploadURL(ownerID int64, uploadID uuid.UUID, partNo int, botSlot int, partBytes int64, leaseToken uuid.UUID) (string, bool) {
	if !s.Enabled() || partNo <= 0 || botSlot <= 0 || partBytes <= 0 || leaseToken == uuid.Nil {
		return "", false
	}
	nonce, err := randomNonce()
	if err != nil {
		slog.Error("gateway.nonce_failed", "error", err)
		return "", false
	}
	payload := Payload{
		Version: TicketVersion, Scope: ScopeUpload, FileID: uploadID.String(),
		UserID: ownerID, Nonce: nonce, ExpiresAt: s.now().Add(s.config.TicketTTL).Unix(),
		PartNo: partNo, BotSlot: botSlot, PartBytes: partBytes, LeaseToken: leaseToken.String(),
	}
	ticket, err := Sign(s.config.Secret, payload)
	if err != nil {
		slog.Error("gateway.ticket_sign_failed", "error", err)
		return "", false
	}
	return strings.TrimRight(strings.TrimSpace(s.config.PublicURL), "/") + "/u/" + ticket, true
}

func randomNonce() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
