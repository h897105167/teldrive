-- +goose Up

-- 下载数据面卸载需要按 part 记录存储后端：
-- 存量 part 一律为 mtproto（保持默认值，无法迁移，因为 Bot API 不能由 message_id 反查 file_id）；
-- 新的 bot kind part 记录上传该 part 的 Telegram bot 数字 ID，以及该 bot 视角下的 file_id。
--
-- 这段 DDL 是可重入的。它最初以 000010 发布给过一个既有部署，而同一天上游把
-- 000010 分配给了 user_preferences；合并后本迁移顺延到 000013。已经按旧编号
-- 应用过该 DDL 的数据库会再遇到一次同样的语句，可重入让全新库与既有库都能通过，
-- 而不是在升级时因为 "type already exists" 停下。

-- +goose StatementBegin
DO $$
BEGIN
    CREATE TYPE /* TEMPLATE: schema */part_storage_kind AS ENUM ('mtproto', 'bot');
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
-- +goose StatementEnd

ALTER TABLE /* TEMPLATE: schema */file_parts
    ADD COLUMN IF NOT EXISTS storage_kind /* TEMPLATE: schema */part_storage_kind NOT NULL DEFAULT 'mtproto',
    ADD COLUMN IF NOT EXISTS bot_id BIGINT,
    ADD COLUMN IF NOT EXISTS telegram_file_id TEXT;

-- +goose StatementBegin
DO $$
BEGIN
    ALTER TABLE /* TEMPLATE: schema */file_parts
        ADD CONSTRAINT file_parts_bot_storage_check CHECK (
            (storage_kind = 'mtproto' AND bot_id IS NULL AND telegram_file_id IS NULL)
            OR (storage_kind = 'bot' AND bot_id IS NOT NULL AND telegram_file_id IS NOT NULL)
        );
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
-- +goose StatementEnd

ALTER TABLE /* TEMPLATE: schema */upload_parts
    ADD COLUMN IF NOT EXISTS storage_kind /* TEMPLATE: schema */part_storage_kind NOT NULL DEFAULT 'mtproto',
    ADD COLUMN IF NOT EXISTS bot_id BIGINT,
    ADD COLUMN IF NOT EXISTS telegram_file_id TEXT;

-- 上传中的 bot part 在 claim 时只有 bot_id、尚无 file_id，所以此处只要求 bot_id 存在；
-- file_id 的完整性由 stored 状态约束保证。
-- +goose StatementBegin
DO $$
BEGIN
    ALTER TABLE /* TEMPLATE: schema */upload_parts
        ADD CONSTRAINT upload_parts_bot_storage_check CHECK (
            (storage_kind = 'mtproto' AND bot_id IS NULL AND telegram_file_id IS NULL)
            OR (storage_kind = 'bot' AND bot_id IS NOT NULL)
        );
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
    ALTER TABLE /* TEMPLATE: schema */upload_parts
        ADD CONSTRAINT upload_parts_bot_file_id_check CHECK (
            storage_kind <> 'bot' OR state <> 'stored' OR telegram_file_id IS NOT NULL
        );
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;
-- +goose StatementEnd

-- +goose Down

ALTER TABLE /* TEMPLATE: schema */upload_parts
    DROP CONSTRAINT IF EXISTS upload_parts_bot_file_id_check,
    DROP CONSTRAINT IF EXISTS upload_parts_bot_storage_check,
    DROP COLUMN IF EXISTS telegram_file_id,
    DROP COLUMN IF EXISTS bot_id,
    DROP COLUMN IF EXISTS storage_kind;

ALTER TABLE /* TEMPLATE: schema */file_parts
    DROP CONSTRAINT IF EXISTS file_parts_bot_storage_check,
    DROP COLUMN IF EXISTS telegram_file_id,
    DROP COLUMN IF EXISTS bot_id,
    DROP COLUMN IF EXISTS storage_kind;

DROP TYPE IF EXISTS /* TEMPLATE: schema */part_storage_kind;
