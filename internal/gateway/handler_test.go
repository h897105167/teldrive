package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/uploads"
)

// fakeCompleter 记录 StorePartBot 入参并返回预设错误。
type fakeCompleter struct {
	got *uploads.StorePartBotInput
	err error
}

func (f *fakeCompleter) StorePartBot(_ context.Context, in uploads.StorePartBotInput) (*sqlcgen.UploadPart, error) {
	copied := in
	f.got = &copied
	if f.err != nil {
		return nil, f.err
	}
	return &sqlcgen.UploadPart{}, nil
}

// handler_test.go —— 校验 manifest 端点的状态码映射与响应体脱敏。

func manifestRequest(method, authorization string) *http.Request {
	request := httptest.NewRequest(method, "/internal/gateway/manifest", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return request
}

func downloadTicket(t *testing.T, service *Service, expiresAt int64) string {
	t.Helper()
	ticket, err := Sign(service.config.Secret, Payload{
		Version: TicketVersion, Scope: ScopeDownload, FileID: testFileID.String(),
		UserID: 7, Nonce: "nonce", ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return ticket
}

func TestManifestHandlerReturnsManifest(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	service := testService(&fakeStore{file: file, parts: parts})
	handler := service.ManifestHandler()
	ticket := downloadTicket(t, service, service.now().Add(time.Minute).Unix())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, manifestRequest(http.MethodGet, "Bearer "+ticket))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}
	var manifest Manifest
	if err := json.Unmarshal(recorder.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(manifest.Parts) != 3 || manifest.Parts[2].Offset != 2*PartSize {
		t.Fatalf("manifest = %+v", manifest)
	}
	if body := recorder.Body.String(); strings.Contains(body, service.config.Secret) {
		t.Fatal("manifest body leaked the gateway secret")
	}
}

func TestManifestHandlerStatusCodes(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	okStore := func() Store { return &fakeStore{file: file, parts: parts} }

	tests := []struct {
		name          string
		store         Store
		method        string
		authorization func(*testing.T, *Service) string
		want          int
	}{
		{name: "missing authorization", store: okStore(), method: http.MethodGet, want: http.StatusUnauthorized},
		{name: "non bearer scheme", store: okStore(), method: http.MethodGet,
			authorization: func(*testing.T, *Service) string { return "Basic abc" }, want: http.StatusUnauthorized},
		{name: "malformed ticket", store: okStore(), method: http.MethodGet,
			authorization: func(*testing.T, *Service) string { return "Bearer nonsense" }, want: http.StatusBadRequest},
		{name: "tampered ticket", store: okStore(), method: http.MethodGet,
			authorization: func(t *testing.T, service *Service) string {
				return "Bearer " + downloadTicket(t, service, service.now().Add(time.Minute).Unix()) + "x"
			}, want: http.StatusForbidden},
		{name: "expired ticket", store: okStore(), method: http.MethodGet,
			authorization: func(t *testing.T, service *Service) string {
				return "Bearer " + downloadTicket(t, service, service.now().Unix())
			}, want: http.StatusGone},
		{name: "upload scope ticket", store: okStore(), method: http.MethodGet,
			authorization: func(t *testing.T, service *Service) string {
				ticket, err := Sign(service.config.Secret, Payload{
					Version: TicketVersion, Scope: ScopeUpload, FileID: testFileID.String(),
					UserID: 7, Nonce: "nonce", ExpiresAt: service.now().Add(time.Minute).Unix(),
				})
				if err != nil {
					t.Fatalf("Sign: %v", err)
				}
				return "Bearer " + ticket
			}, want: http.StatusBadRequest},
		{name: "unknown file", store: &fakeStore{err: ErrFileNotFound}, method: http.MethodGet,
			authorization: func(t *testing.T, service *Service) string {
				return "Bearer " + downloadTicket(t, service, service.now().Add(time.Minute).Unix())
			}, want: http.StatusNotFound},
		{name: "unsupported layout", store: &fakeStore{file: file, parts: []sqlcgen.FilePart{mtprotoPart(1, PartSize)}},
			method: http.MethodGet,
			authorization: func(t *testing.T, service *Service) string {
				return "Bearer " + downloadTicket(t, service, service.now().Add(time.Minute).Unix())
			}, want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := testService(test.store)
			authorization := ""
			if test.authorization != nil {
				authorization = test.authorization(t, service)
			}
			recorder := httptest.NewRecorder()
			service.ManifestHandler().ServeHTTP(recorder, manifestRequest(test.method, authorization))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
		})
	}
}

func TestManifestHandlerRejectsUnsupportedMethods(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	service := testService(&fakeStore{file: file, parts: parts})
	ticket := downloadTicket(t, service, service.now().Add(time.Minute).Unix())

	recorder := httptest.NewRecorder()
	service.ManifestHandler().ServeHTTP(recorder, manifestRequest(http.MethodPost, "Bearer "+ticket))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("allow = %q, want GET, HEAD", got)
	}
}

func TestManifestHandlerHeadHasEmptyBody(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	service := testService(&fakeStore{file: file, parts: parts})
	ticket := downloadTicket(t, service, service.now().Add(time.Minute).Unix())

	recorder := httptest.NewRecorder()
	service.ManifestHandler().ServeHTTP(recorder, manifestRequest(http.MethodHead, "Bearer "+ticket))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

func uploadTicket(t *testing.T, service *Service, uploadID, leaseToken uuid.UUID, expiresAt int64) string {
	t.Helper()
	ticket, err := Sign(service.config.Secret, Payload{
		Version: TicketVersion, Scope: ScopeUpload, FileID: uploadID.String(),
		UserID: 7, Nonce: "nonce", ExpiresAt: expiresAt,
		PartNo: 3, BotSlot: 2, PartBytes: PartSize, LeaseToken: leaseToken.String(),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return ticket
}

func newUploadCompleteRequest(method, authorization, body string) *http.Request {
	request := httptest.NewRequest(method, "/internal/gateway/upload-complete", strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return request
}

func TestUploadCompleteHandlerStoresPart(t *testing.T) {
	t.Parallel()
	service := testService(&fakeStore{})
	completer := &fakeCompleter{}
	uploadID := uuid.New()
	leaseToken := uuid.New()
	ticket := uploadTicket(t, service, uploadID, leaseToken, service.now().Add(time.Minute).Unix())
	body := `{"fid":"telegram-file-id","mid":123,"size":16777216}`

	recorder := httptest.NewRecorder()
	service.UploadCompleteHandler(completer).ServeHTTP(
		recorder, newUploadCompleteRequest(http.MethodPost, "Bearer "+ticket, body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
	}
	if completer.got == nil {
		t.Fatal("StorePartBot was not called")
	}
	got := *completer.got
	if got.UploadID != uploadID || got.PartNo != 3 || got.LeaseToken != leaseToken ||
		got.MessageID != 123 || got.StoredSize != PartSize || got.TelegramFileID != "telegram-file-id" {
		t.Fatalf("StorePartBot input = %+v", got)
	}
	if strings.Contains(recorder.Body.String(), "telegram-file-id") {
		t.Fatal("response body leaked the telegram file id")
	}
}

func TestUploadCompleteHandlerStatusCodes(t *testing.T) {
	t.Parallel()
	service := testService(&fakeStore{})
	uploadID := uuid.New()
	leaseToken := uuid.New()
	validTicket := func() string {
		return uploadTicket(t, service, uploadID, leaseToken, service.now().Add(time.Minute).Unix())
	}
	validBody := `{"fid":"telegram-file-id","mid":123,"size":16777216}`

	downloadScopeTicket, err := Sign(service.config.Secret, Payload{
		Version: TicketVersion, Scope: ScopeDownload, FileID: uploadID.String(),
		UserID: 7, Nonce: "nonce", ExpiresAt: service.now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tests := []struct {
		name          string
		method        string
		authorization string
		body          string
		completerErr  error
		want          int
	}{
		{name: "wrong method", method: http.MethodGet, authorization: "Bearer " + validTicket(), body: validBody, want: http.StatusMethodNotAllowed},
		{name: "missing authorization", method: http.MethodPost, body: validBody, want: http.StatusUnauthorized},
		{name: "tampered ticket", method: http.MethodPost, authorization: "Bearer " + validTicket() + "x", body: validBody, want: http.StatusForbidden},
		{name: "expired ticket", method: http.MethodPost,
			authorization: "Bearer " + uploadTicket(t, service, uploadID, leaseToken, service.now().Unix()),
			body:          validBody, want: http.StatusGone},
		{name: "download scope ticket", method: http.MethodPost, authorization: "Bearer " + downloadScopeTicket, body: validBody, want: http.StatusBadRequest},
		{name: "size mismatch", method: http.MethodPost, authorization: "Bearer " + validTicket(),
			body: `{"fid":"x","mid":1,"size":42}`, want: http.StatusBadRequest},
		{name: "empty file id", method: http.MethodPost, authorization: "Bearer " + validTicket(),
			body: `{"fid":"","mid":1,"size":16777216}`, want: http.StatusBadRequest},
		{name: "malformed json", method: http.MethodPost, authorization: "Bearer " + validTicket(),
			body: `{`, want: http.StatusBadRequest},
		{name: "lease lost", method: http.MethodPost, authorization: "Bearer " + validTicket(),
			body: validBody, completerErr: uploads.ErrLeaseLost, want: http.StatusConflict},
		{name: "invalid input", method: http.MethodPost, authorization: "Bearer " + validTicket(),
			body: validBody, completerErr: uploads.ErrInvalidInput, want: http.StatusBadRequest},
		{name: "internal error", method: http.MethodPost, authorization: "Bearer " + validTicket(),
			body: validBody, completerErr: errors.New("boom"), want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			completer := &fakeCompleter{err: test.completerErr}
			recorder := httptest.NewRecorder()
			service.UploadCompleteHandler(completer).ServeHTTP(
				recorder, newUploadCompleteRequest(test.method, test.authorization, test.body))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, body = %q, want %d", recorder.Code, recorder.Body.String(), test.want)
			}
		})
	}
}
