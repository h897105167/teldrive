package gateway

// handler.go —— 内部 manifest 端点：Worker 用下载 ticket 自证身份后拉取清单。
// 响应体只包含清单本身，绝不出现 bot token、file_path 或任何内部 URL。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

// UploadCompleter 抽象上传完成落库能力，生产实现为 uploads.Service。
type UploadCompleter interface {
	StorePartBot(ctx context.Context, in uploads.StorePartBotInput) (*sqlcgen.UploadPart, error)
}

// ManifestHandler 返回 /internal/gateway/manifest 的处理器。
func (s *Service) ManifestHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeManifestError(w, http.StatusMethodNotAllowed)
			return
		}
		ticket := bearerToken(r.Header.Get("Authorization"))
		if ticket == "" {
			writeManifestError(w, http.StatusUnauthorized)
			return
		}
		payload, err := Verify(s.config.Secret, ticket, s.now())
		switch {
		case errors.Is(err, ErrTicketExpired):
			writeManifestError(w, http.StatusGone)
			return
		case errors.Is(err, ErrTicketSignatureInvalid):
			writeManifestError(w, http.StatusForbidden)
			return
		case err != nil:
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		if payload.Scope != ScopeDownload {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		fileID, err := uuid.Parse(payload.FileID)
		if err != nil {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		manifest, err := s.LoadManifest(r.Context(), fileID, payload.UserID)
		if err != nil {
			if errors.Is(err, ErrFileNotFound) {
				writeManifestError(w, http.StatusNotFound)
				return
			}
			slog.ErrorContext(r.Context(), "gateway.manifest_failed", "file_id", fileID, "error", err)
			writeManifestError(w, http.StatusInternalServerError)
			return
		}
		writeManifest(w, r, manifest)
	})
}

func writeManifest(w http.ResponseWriter, r *http.Request, manifest *Manifest) {
	body, err := json.Marshal(manifest)
	if err != nil {
		writeManifestError(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// writeManifestError 只输出稳定的机器可读错误码，与 Worker 的错误契约一致。
func writeManifestError(w http.ResponseWriter, status int) {
	code := "internal_error"
	switch status {
	case http.StatusUnauthorized:
		code = "ticket_missing"
	case http.StatusBadRequest:
		code = "ticket_invalid"
	case http.StatusForbidden:
		code = ErrTicketSignatureInvalid.Error()
	case http.StatusGone:
		code = ErrTicketExpired.Error()
	case http.StatusNotFound:
		code = ErrFileNotFound.Error()
	case http.StatusConflict:
		code = "lease_lost"
	case http.StatusMethodNotAllowed:
		code = "method_not_allowed"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func bearerToken(header string) string {
	scheme, value, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// uploadCompleteRequest 是 Worker 回调的 JSON 体，字段全部来自 Telegram sendDocument 结果。
type uploadCompleteRequest struct {
	FileID    string `json:"fid"`
	MessageID int64  `json:"mid"`
	Size      int64  `json:"size"`
}

// UploadCompleteHandler 返回 /internal/gateway/upload-complete 的处理器：
// Worker 用上传 ticket（Bearer 自证）回报分块落库结果。
// telegram_file_id 只写入数据库，绝不出现在响应体中。
func (s *Service) UploadCompleteHandler(completer UploadCompleter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeManifestError(w, http.StatusMethodNotAllowed)
			return
		}
		ticket := bearerToken(r.Header.Get("Authorization"))
		if ticket == "" {
			writeManifestError(w, http.StatusUnauthorized)
			return
		}
		payload, err := Verify(s.config.Secret, ticket, s.now())
		switch {
		case errors.Is(err, ErrTicketExpired):
			writeManifestError(w, http.StatusGone)
			return
		case errors.Is(err, ErrTicketSignatureInvalid):
			writeManifestError(w, http.StatusForbidden)
			return
		case err != nil:
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		if payload.Scope != ScopeUpload || payload.PartNo <= 0 || payload.BotSlot <= 0 ||
			payload.PartBytes <= 0 || payload.LeaseToken == "" {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		uploadID, err := uuid.Parse(payload.FileID)
		if err != nil {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		leaseToken, err := uuid.Parse(payload.LeaseToken)
		if err != nil {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		var body uploadCompleteRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		// 明文字节数必须与票据声明一致：Worker 已校验请求体长度，此处复核 Telegram 回报值。
		if strings.TrimSpace(body.FileID) == "" || body.MessageID <= 0 || body.Size != payload.PartBytes {
			writeManifestError(w, http.StatusBadRequest)
			return
		}
		_, err = completer.StorePartBot(r.Context(), uploads.StorePartBotInput{
			UploadID: uploadID, PartNo: int32(payload.PartNo), LeaseToken: leaseToken,
			MessageID: body.MessageID, StoredSize: body.Size, TelegramFileID: strings.TrimSpace(body.FileID),
		})
		switch {
		case errors.Is(err, uploads.ErrLeaseLost):
			writeManifestError(w, http.StatusConflict)
			return
		case errors.Is(err, uploads.ErrInvalidInput):
			writeManifestError(w, http.StatusBadRequest)
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "gateway.upload_complete_failed", "error", err)
			writeManifestError(w, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "stored"})
	})
}
