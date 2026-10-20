package api

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/authn"
	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/channels"
	"github.com/tgdrive/teldrive/v2/internal/config"
	"github.com/tgdrive/teldrive/v2/internal/events"
	"github.com/tgdrive/teldrive/v2/internal/fileops"
	"github.com/tgdrive/teldrive/v2/internal/gateway"
	"github.com/tgdrive/teldrive/v2/internal/health"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	"github.com/tgdrive/teldrive/v2/internal/shares"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	"github.com/tgdrive/teldrive/v2/internal/transfer"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

var ErrOperationUnavailable = errors.New("operation is not implemented")

type Handler struct {
	Catalog                    *catalog.Service
	Uploads                    *uploads.Service
	UploadPipeline             *transfer.Pipeline
	Downloader                 *transfer.Downloader
	Events                     *events.Service
	Health                     *health.Service
	Jobs                       *jobs.Runtime
	Auth                       *authn.Service
	Bots                       *bots.Service
	Channels                   *channels.Service
	FileOps                    *fileops.Service
	Shares                     *shares.Service
	TelegramAccount            telegramstore.Account
	ActiveEncryptionKeyVersion int32
	// Gateway 是可选的下载数据面卸载；nil 时全部下载仍由本进程直连 Telegram。
	Gateway *gateway.Service

	// Cloudflare 一键部署所需；nil 表示该功能未启用。
	CloudflareCipher *secureblob.Cipher
	ConfigPath       string
	RuntimeConfig    *config.Config
	Pool             *pgxpool.Pool
}

func NewHandler(catalogService *catalog.Service, uploadService *uploads.Service, uploadPipeline *transfer.Pipeline, downloader *transfer.Downloader, healthService *health.Service, activeEncryptionKeyVersion int32, eventService *events.Service) *Handler {
	return &Handler{
		Catalog: catalogService, Uploads: uploadService, UploadPipeline: uploadPipeline,
		Downloader: downloader, Events: eventService, Health: healthService,
		ActiveEncryptionKeyVersion: activeEncryptionKeyVersion,
	}
}

func (h *Handler) ConfigureDomains(authService *authn.Service, botService *bots.Service, channelService *channels.Service, fileService *fileops.Service, shareService *shares.Service, account telegramstore.Account) *Handler {
	if h != nil {
		h.Auth, h.Bots, h.Channels, h.FileOps, h.Shares = authService, botService, channelService, fileService, shareService
		h.TelegramAccount = account
	}
	return h
}

func (h *Handler) ConfigureJobs(runtime *jobs.Runtime) *Handler {
	if h != nil {
		h.Jobs = runtime
	}
	return h
}

// ConfigureGateway attaches the optional download data-plane gateway; a nil or
// disabled service keeps every download on the direct VPS path.
func (h *Handler) ConfigureGateway(service *gateway.Service) *Handler {
	if h != nil {
		h.Gateway = service
	}
	return h
}

// ConfigurePool wires the PostgreSQL connection pool needed by settings
// endpoints (bot token lookup).
func (h *Handler) ConfigurePool(pool *pgxpool.Pool) *Handler {
	if h != nil {
		h.Pool = pool
	}
	return h
}

var _ gen.Handler = (*Handler)(nil)
