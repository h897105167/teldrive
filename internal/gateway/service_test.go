package gateway

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

// service_test.go —— 用假 Store 校验 302 签发、回退与清单加载，不依赖数据库。

type fakeStore struct {
	file  sqlcgen.File
	parts []sqlcgen.FilePart
	err   error
}

func (f *fakeStore) GetFileForUser(context.Context, uuid.UUID, int64) (sqlcgen.File, error) {
	if f.err != nil {
		return sqlcgen.File{}, f.err
	}
	return f.file, nil
}

func (f *fakeStore) ListFileParts(context.Context, uuid.UUID) ([]sqlcgen.FilePart, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.parts, nil
}

func testService(store Store) *Service {
	service := New(store, Config{
		Enabled: true, PublicURL: "https://gw.workers.dev/", Secret: "shared-secret",
		TicketTTL: 10 * time.Minute, BotSlots: testSlots,
	})
	service.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return service
}

func TestProxyDownloadSignsWorkerLocation(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	service := testService(&fakeStore{file: file, parts: parts})

	location, ok := service.ProxyDownload(context.Background(), 7, testFileID, &file, "0-1048575", true)
	if !ok {
		t.Fatal("ProxyDownload ok = false, want true")
	}
	const prefix = "https://gw.workers.dev/d/"
	if !strings.HasPrefix(location, prefix) {
		t.Fatalf("location = %q, want prefix %q", location, prefix)
	}
	payload, err := Verify("shared-secret", strings.TrimPrefix(location, prefix), service.now())
	if err != nil {
		t.Fatalf("Verify location: %v", err)
	}
	if payload.Scope != ScopeDownload || payload.FileID != testFileID.String() || payload.UserID != 7 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Range != "0-1048575" || payload.Attachment != 1 || payload.Nonce == "" {
		t.Fatalf("payload range/attachment/nonce = %+v", payload)
	}
	if want := service.now().Add(10 * time.Minute).Unix(); payload.ExpiresAt != want {
		t.Fatalf("payload expires = %d, want %d", payload.ExpiresAt, want)
	}
}

func TestProxyDownloadOmitsRangeAndAttachmentWhenAbsent(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	service := testService(&fakeStore{file: file, parts: parts})

	location, ok := service.ProxyDownload(context.Background(), 7, testFileID, &file, "", false)
	if !ok {
		t.Fatal("ProxyDownload ok = false, want true")
	}
	payload, err := Verify("shared-secret", strings.TrimPrefix(location, "https://gw.workers.dev/d/"), service.now())
	if err != nil {
		t.Fatalf("Verify location: %v", err)
	}
	if payload.Range != "" || payload.Attachment != 0 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestProxyDownloadSplitsByFileSize(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	largeStore := &fakeStore{file: file, parts: parts}

	// 小文件必须有配套的单分片布局，否则 Eligible 会先于大小分流拒绝。
	small := file
	small.Size = pgtype.Int8{Int64: PartSize / 16, Valid: true}
	smallParts := []sqlcgen.FilePart{botPart(1, 8430828351, PartSize/16)}
	smallStore := &fakeStore{file: small, parts: smallParts}

	tests := map[string]struct {
		store *fakeStore
		file  *sqlcgen.File
	}{
		"large file over threshold":  {store: largeStore, file: &file},
		"small file under threshold": {store: smallStore, file: &small},
		"zero threshold keeps small": {store: smallStore, file: &small},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			service := testService(test.store)
			if name == "zero threshold keeps small" {
				service.SetWorkerMinBytes(0)
			} else {
				service.SetWorkerMinBytes(PartSize)
			}
			location, ok := service.ProxyDownload(context.Background(), 7, testFileID, test.file, "", false)
			want := name != "small file under threshold"
			if ok != want {
				t.Fatalf("ProxyDownload ok = %v, want %v", ok, want)
			}
			if ok && !strings.HasPrefix(location, "https://gw.workers.dev/d/") {
				t.Fatalf("location = %q, want worker prefix", location)
			}
		})
	}
}

func TestProxyDownloadFallsBack(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	mtprotoOnly := []sqlcgen.FilePart{mtprotoPart(1, PartSize)}
	encrypted := file
	encrypted.Encryption = true

	tests := map[string]struct {
		service *Service
		file    *sqlcgen.File
		parts   []sqlcgen.FilePart
	}{
		"disabled gateway":   {service: New(&fakeStore{file: file, parts: parts}, Config{PublicURL: "https://gw.workers.dev", Secret: "s"}), file: &file, parts: parts},
		"nil gateway":        {service: nil, file: &file, parts: parts},
		"mtproto only parts": {service: testService(&fakeStore{file: file, parts: mtprotoOnly}), file: &file, parts: mtprotoOnly},
		"encrypted file":     {service: testService(&fakeStore{file: encrypted, parts: parts}), file: &encrypted, parts: parts},
		"store failure":      {service: testService(&fakeStore{err: errors.New("boom")}), file: &file, parts: parts},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			location, ok := test.service.ProxyDownload(context.Background(), 7, testFileID, test.file, "", false)
			if ok || location != "" {
				t.Fatalf("ProxyDownload = (%q, %v), want (\"\", false)", location, ok)
			}
		})
	}
}

func TestLoadManifestReturnsStoreErrors(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	service := testService(&fakeStore{file: file, parts: parts})
	manifest, err := service.LoadManifest(context.Background(), testFileID, 7)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.FileID != testFileID.String() || len(manifest.Parts) != 3 {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestLoadManifestPropagatesMissingFile(t *testing.T) {
	t.Parallel()
	service := testService(&fakeStore{err: ErrFileNotFound})
	if _, err := service.LoadManifest(context.Background(), testFileID, 7); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("LoadManifest = %v, want ErrFileNotFound", err)
	}
}

func TestUploadEligible(t *testing.T) {
	t.Parallel()
	base := &sqlcgen.UploadSession{PartSize: PartSize}
	withEncryption := *base
	withEncryption.Encryption = true
	withHash := *base
	withHash.ExpectedHashAlgorithm = pgtype.Text{String: "sha256", Valid: true}
	wrongPartSize := *base
	wrongPartSize.PartSize = PartSize / 2

	tests := map[string]struct {
		session *sqlcgen.UploadSession
		want    bool
	}{
		"eligible":              {session: base, want: true},
		"nil session":           {session: nil, want: false},
		"encrypted":             {session: &withEncryption, want: false},
		"expected hash declared": {session: &withHash, want: false},
		"non 16MiB part size":   {session: &wrongPartSize, want: false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := UploadEligible(test.session); got != test.want {
				t.Fatalf("UploadEligible = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSelectBotSlot(t *testing.T) {
	t.Parallel()
	service := testService(&fakeStore{})
	botID, slot, ok := service.SelectBotSlot()
	if !ok {
		t.Fatal("SelectBotSlot ok = false, want true")
	}
	if slot < 1 || slot > len(testSlots) {
		t.Fatalf("slot = %d, want within [1,%d]", slot, len(testSlots))
	}
	want, err := strconv.ParseInt(testSlots[slot-1], 10, 64)
	if err != nil || want <= 0 {
		t.Fatalf("test slot %d is misconfigured: %q", slot, testSlots[slot-1])
	}
	if botID != want {
		t.Fatalf("botID = %d, want %d (slot %d)", botID, want, slot)
	}
}

func TestSelectBotSlotDisabled(t *testing.T) {
	t.Parallel()
	disabled := New(&fakeStore{}, Config{PublicURL: "https://gw.workers.dev", Secret: "s"})
	if botID, slot, ok := disabled.SelectBotSlot(); ok || botID != 0 || slot != 0 {
		t.Fatalf("SelectBotSlot = (%d, %d, %v), want (0, 0, false)", botID, slot, ok)
	}
}

func TestUploadURLSignsWorkerUploadLocation(t *testing.T) {
	t.Parallel()
	service := testService(&fakeStore{})
	uploadID := uuid.New()
	leaseToken := uuid.New()

	location, ok := service.UploadURL(7, uploadID, 3, 2, PartSize, leaseToken)
	if !ok {
		t.Fatal("UploadURL ok = false, want true")
	}
	const prefix = "https://gw.workers.dev/u/"
	if !strings.HasPrefix(location, prefix) {
		t.Fatalf("location = %q, want prefix %q", location, prefix)
	}
	payload, err := Verify("shared-secret", strings.TrimPrefix(location, prefix), service.now())
	if err != nil {
		t.Fatalf("Verify location: %v", err)
	}
	if payload.Scope != ScopeUpload || payload.FileID != uploadID.String() || payload.UserID != 7 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.PartNo != 3 || payload.BotSlot != 2 || payload.PartBytes != PartSize ||
		payload.LeaseToken != leaseToken.String() || payload.Nonce == "" {
		t.Fatalf("payload upload fields = %+v", payload)
	}
	if want := service.now().Add(10 * time.Minute).Unix(); payload.ExpiresAt != want {
		t.Fatalf("payload expires = %d, want %d", payload.ExpiresAt, want)
	}
}

func TestUploadURLRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	service := testService(&fakeStore{})
	uploadID := uuid.New()
	lease := uuid.New()

	tests := map[string]struct {
		service   *Service
		partNo    int
		botSlot   int
		partBytes int64
		lease     uuid.UUID
	}{
		"disabled":       {service: New(&fakeStore{}, Config{PublicURL: "https://gw.workers.dev", Secret: "s"}), partNo: 1, botSlot: 1, partBytes: PartSize, lease: lease},
		"zero part no":   {service: service, partNo: 0, botSlot: 1, partBytes: PartSize, lease: lease},
		"zero bot slot":  {service: service, partNo: 1, botSlot: 0, partBytes: PartSize, lease: lease},
		"zero part size": {service: service, partNo: 1, botSlot: 1, partBytes: 0, lease: lease},
		"nil lease":      {service: service, partNo: 1, botSlot: 1, partBytes: PartSize, lease: uuid.Nil},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			location, ok := test.service.UploadURL(7, uploadID, test.partNo, test.botSlot, test.partBytes, test.lease)
			if ok || location != "" {
				t.Fatalf("UploadURL = (%q, %v), want (\"\", false)", location, ok)
			}
		})
	}
}
