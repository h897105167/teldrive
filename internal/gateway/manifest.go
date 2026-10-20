package gateway

// manifest.go —— 组装数据面所需的文件分片清单，字段必须与 worker/src/manifest.ts 逐字节一致。

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
)

// PartSize 为网关约定的单片明文尺寸：Telegram Bot API 下载上限 20 MiB，16 MiB 留出余量。
const PartSize = 16 << 20

// ManifestVersion 是当前清单结构版本，Worker 只接受该值。
const ManifestVersion = 1

// ManifestPart 描述一个 part 在明文全文件中的位置及其 Telegram 归属。
type ManifestPart struct {
	Number int    `json:"n"`
	Offset int64  `json:"off"`
	Length int64  `json:"len"`
	Bot    int    `json:"bot"`
	FileID string `json:"fid"`
}

// Manifest 是 Worker 拉取的完整分片清单。
type Manifest struct {
	Version      int            `json:"v"`
	FileID       string         `json:"fileId"`
	Name         string         `json:"name"`
	Size         int64          `json:"size"`
	ContentType  string         `json:"contentType"`
	ETag         string         `json:"etag"`
	LastModified string         `json:"lastModified"`
	PartSize     int64          `json:"partSize"`
	Encrypted    bool           `json:"encrypted"`
	Parts        []ManifestPart `json:"parts"`
}

// Eligible 判断文件是否可以完全经由网关下载：要求文件为 active 的普通文件、未加密、
// parts 非空，且每个 part 都是 bot 存储、bot_id 能在槽位表中定位、file_id 非空，
// part_no 从 1 连续到最后，plain_size 有效且累加等于文件大小。
// 存量 mtproto part 一律不满足，因此这些文件自动走 VPS 直连路径。
func Eligible(file *sqlcgen.File, parts []sqlcgen.FilePart, botSlots []string) bool {
	if file == nil || file.Kind != sqlcgen.FileKindFile || file.Status != sqlcgen.FileStatusActive {
		return false
	}
	if !file.Size.Valid || file.Size.Int64 < 0 || file.Encryption || len(parts) == 0 {
		return false
	}
	slots := botSlotIndex(botSlots)
	ordered := partsByNumber(parts)
	var total int64
	for index, part := range ordered {
		if part.PartNo != int32(index+1) || part.StorageKind != sqlcgen.PartStorageKindBot {
			return false
		}
		if !part.BotID.Valid || !part.TelegramFileID.Valid || part.BotID.Int64 <= 0 {
			return false
		}
		if strings.TrimSpace(part.TelegramFileID.String) == "" {
			return false
		}
		if _, ok := slots[strconv.FormatInt(part.BotID.Int64, 10)]; !ok {
			return false
		}
		if !part.PlainSize.Valid || part.PlainSize.Int64 < 0 {
			return false
		}
		total += part.PlainSize.Int64
	}
	return total == file.Size.Int64
}

// BuildManifest 生成分片清单：off 为前面所有 part 的 plain_size 累加，len 为该 part 的
// plain_size，bot 为 bot_id 在槽位表中的序号（index+1），fid 为该 bot 视角下的 file_id。
func BuildManifest(file *sqlcgen.File, parts []sqlcgen.FilePart, botSlots []string, etag string) (*Manifest, error) {
	if !Eligible(file, parts, botSlots) {
		return nil, ErrNotEligible
	}
	slots := botSlotIndex(botSlots)
	ordered := partsByNumber(parts)
	manifestParts := make([]ManifestPart, 0, len(ordered))
	var offset int64
	for _, part := range ordered {
		manifestParts = append(manifestParts, ManifestPart{
			Number: int(part.PartNo), Offset: offset, Length: part.PlainSize.Int64,
			Bot: slots[strconv.FormatInt(part.BotID.Int64, 10)], FileID: part.TelegramFileID.String,
		})
		offset += part.PlainSize.Int64
	}
	contentType := "application/octet-stream"
	if file.MimeType.Valid && strings.TrimSpace(file.MimeType.String) != "" {
		contentType = file.MimeType.String
	}
	fileID, _ := dbtypes.GoogleUUID(file.ID)
	return &Manifest{
		Version:      ManifestVersion,
		FileID:       fileID.String(),
		Name:         file.Name,
		Size:         file.Size.Int64,
		ContentType:  contentType,
		ETag:         etag,
		LastModified: file.ModTime.Time.UTC().Format(http.TimeFormat),
		PartSize:     PartSize,
		Encrypted:    file.Encryption,
		Parts:        manifestParts,
	}, nil
}

// contentETag 必须与 internal/api.contentETag 保持一致，客户端才能跨 VPS / 网关两条路径复用缓存。
func contentETag(file *sqlcgen.File) string {
	if file.HashValue.Valid && strings.TrimSpace(file.HashValue.String) != "" {
		return `"` + file.HashValue.String + `"`
	}
	return fmt.Sprintf(`"%d"`, file.Generation)
}

// botSlotIndex 把配置的 bot 数字 ID 映射为 1 基槽位号。
func botSlotIndex(botSlots []string) map[string]int {
	slots := make(map[string]int, len(botSlots))
	for index, slot := range botSlots {
		slots[strings.TrimSpace(slot)] = index + 1
	}
	return slots
}

func partsByNumber(parts []sqlcgen.FilePart) []sqlcgen.FilePart {
	ordered := append([]sqlcgen.FilePart(nil), parts...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].PartNo < ordered[right].PartNo })
	return ordered
}
