package gateway

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

// manifest_test.go —— 校验网关可选性判定与清单布局。

var (
	testFileID = uuid.MustParse("0f2c1d2e-0000-4000-8000-000000000000")
	testSlots  = []string{"8430828351", "8663276280"}
)

func testFile() sqlcgen.File {
	return sqlcgen.File{
		ID:       pgtype.UUID{Bytes: testFileID, Valid: true},
		UserID:   7,
		Name:     "movie.mkv",
		Kind:     sqlcgen.FileKindFile,
		MimeType: pgtype.Text{String: "video/x-matroska", Valid: true},
		Status:   sqlcgen.FileStatusActive,
		Size:     pgtype.Int8{Int64: 3 * PartSize, Valid: true},
		// contentETag 优先使用 hash_value，必须与 internal/api 保持一致。
		HashValue: pgtype.Text{String: "a1b2c3", Valid: true},
		ModTime:   pgtype.Timestamptz{Time: time.Date(2026, time.October, 15, 3, 4, 5, 0, time.UTC), Valid: true},
	}
}

func botPart(partNo int32, botID int64, plainSize int64) sqlcgen.FilePart {
	return sqlcgen.FilePart{
		FileID:         pgtype.UUID{Bytes: testFileID, Valid: true},
		PartNo:         partNo,
		ChannelID:      1000,
		MessageID:      int64(partNo),
		PlainSize:      pgtype.Int8{Int64: plainSize, Valid: true},
		StoredSize:     pgtype.Int8{Int64: plainSize, Valid: true},
		CreatedAt:      pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true},
		StorageKind:    sqlcgen.PartStorageKindBot,
		BotID:          pgtype.Int8{Int64: botID, Valid: true},
		TelegramFileID: pgtype.Text{String: fmt.Sprintf("BQACAgQAAxk%d", partNo), Valid: true},
	}
}

func mtprotoPart(partNo int32, plainSize int64) sqlcgen.FilePart {
	part := botPart(partNo, 0, plainSize)
	part.StorageKind = sqlcgen.PartStorageKindMtproto
	part.BotID = pgtype.Int8{}
	part.TelegramFileID = pgtype.Text{}
	return part
}

func eligibleFixture() (sqlcgen.File, []sqlcgen.FilePart) {
	file := testFile()
	parts := []sqlcgen.FilePart{
		botPart(1, 8430828351, PartSize),
		botPart(2, 8663276280, PartSize),
		botPart(3, 8430828351, PartSize),
	}
	return file, parts
}

func TestEligibleAcceptsCompleteBotLayout(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	if !Eligible(&file, parts, testSlots) {
		t.Fatal("Eligible = false, want true")
	}
}

func TestEligibleRejectsUnsupportedLayouts(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*sqlcgen.File, []sqlcgen.FilePart) []sqlcgen.FilePart{
		"encrypted file": func(file *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			file.Encryption = true
			return parts
		},
		"mtproto parts only": func(_ *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			parts[0] = mtprotoPart(1, PartSize)
			return parts
		},
		"mixed storage kinds": func(_ *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			parts[1] = mtprotoPart(2, PartSize)
			return parts
		},
		"unknown bot id": func(_ *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			parts[0] = botPart(1, 1234, PartSize)
			return parts
		},
		"missing file id": func(_ *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			parts[0].TelegramFileID = pgtype.Text{}
			return parts
		},
		"part hole": func(_ *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			parts[1] = botPart(3, 8663276280, PartSize)
			return parts
		},
		"size mismatch": func(file *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			file.Size = pgtype.Int8{Int64: 3*PartSize + 1, Valid: true}
			return parts
		},
		"missing size": func(file *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			file.Size = pgtype.Int8{}
			return parts
		},
		"folder kind": func(file *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			file.Kind = sqlcgen.FileKindFolder
			return parts
		},
		"trashed file": func(file *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			file.Status = sqlcgen.FileStatusTrashed
			return parts
		},
		"no parts": func(_ *sqlcgen.File, _ []sqlcgen.FilePart) []sqlcgen.FilePart {
			return nil
		},
		"invalid plain size": func(_ *sqlcgen.File, parts []sqlcgen.FilePart) []sqlcgen.FilePart {
			parts[0].PlainSize = pgtype.Int8{Int64: -1, Valid: true}
			return parts
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, parts := eligibleFixture()
			parts = mutate(&file, parts)
			if Eligible(&file, parts, testSlots) {
				t.Fatalf("Eligible = true, want false for %s", name)
			}
		})
	}
}

func TestEligibleRequiresConfiguredBotSlots(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	if Eligible(&file, parts, nil) {
		t.Fatal("Eligible = true with no bot slots, want false")
	}
	if Eligible(nil, parts, testSlots) {
		t.Fatal("Eligible = true for nil file, want false")
	}
}

func TestBuildManifestLayoutMatchesWorkerContract(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	manifest, err := BuildManifest(&file, parts, testSlots, contentETag(&file))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if manifest.Version != 1 || manifest.FileID != testFileID.String() || manifest.Name != "movie.mkv" {
		t.Fatalf("manifest header = %+v", manifest)
	}
	if manifest.Size != 3*PartSize || manifest.PartSize != PartSize || manifest.Encrypted {
		t.Fatalf("manifest sizes = %+v", manifest)
	}
	if manifest.ContentType != "video/x-matroska" || manifest.ETag != `"a1b2c3"` {
		t.Fatalf("manifest metadata = %+v", manifest)
	}
	if want := "Thu, 15 Oct 2026 03:04:05 GMT"; manifest.LastModified != want {
		t.Fatalf("manifest lastModified = %q, want %q", manifest.LastModified, want)
	}
	want := []ManifestPart{
		{Number: 1, Offset: 0, Length: PartSize, Bot: 1, FileID: "BQACAgQAAxk1"},
		{Number: 2, Offset: PartSize, Length: PartSize, Bot: 2, FileID: "BQACAgQAAxk2"},
		{Number: 3, Offset: 2 * PartSize, Length: PartSize, Bot: 1, FileID: "BQACAgQAAxk3"},
	}
	if len(manifest.Parts) != len(want) {
		t.Fatalf("manifest parts = %+v", manifest.Parts)
	}
	for index := range want {
		if manifest.Parts[index] != want[index] {
			t.Fatalf("part %d = %+v, want %+v", index, manifest.Parts[index], want[index])
		}
	}
}

func TestBuildManifestSortsPartsAndDefaultsContentType(t *testing.T) {
	t.Parallel()
	file := testFile()
	file.MimeType = pgtype.Text{}
	file.HashValue = pgtype.Text{}
	file.Generation = 9
	parts := []sqlcgen.FilePart{
		botPart(2, 8663276280, PartSize),
		botPart(1, 8430828351, 2*PartSize),
	}
	manifest, err := BuildManifest(&file, parts, testSlots, contentETag(&file))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if manifest.ContentType != "application/octet-stream" || manifest.ETag != `"9"` {
		t.Fatalf("manifest metadata = %+v", manifest)
	}
	if manifest.Parts[0].Number != 1 || manifest.Parts[0].Length != 2*PartSize || manifest.Parts[0].Bot != 1 {
		t.Fatalf("first part = %+v", manifest.Parts[0])
	}
	if manifest.Parts[1].Offset != 2*PartSize {
		t.Fatalf("second part offset = %d", manifest.Parts[1].Offset)
	}
}

func TestBuildManifestRejectsIneligibleFile(t *testing.T) {
	t.Parallel()
	file, parts := eligibleFixture()
	file.Encryption = true
	if _, err := BuildManifest(&file, parts, testSlots, ""); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("BuildManifest = %v, want ErrNotEligible", err)
	}
}
