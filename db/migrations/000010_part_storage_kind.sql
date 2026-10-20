-- +goose Up

-- 下载数据面卸载需要按 part 记录存储后端：
-- 存量 part 一律为 mtproto（保持默认值，无法迁移，因为 Bot API 不能由 message_id 反查 file_id）；
-- 新的 bot kind part 记录上传该 part 的 Telegram bot 数字 ID，以及该 bot 视角下的 file_id。
CREATE TYPE /* TEMPLATE: schema */part_storage_kind AS ENUM ('mtproto', 'bot');

ALTER TABLE /* TEMPLATE: schema */file_parts
    ADD COLUMN storage_kind /* TEMPLATE: schema */part_storage_kind NOT NULL DEFAULT 'mtproto',
    ADD COLUMN bot_id BIGINT,
    ADD COLUMN telegram_file_id TEXT;

ALTER TABLE /* TEMPLATE: schema */file_parts
    ADD CONSTRAINT file_parts_bot_storage_check CHECK (
        (storage_kind = 'mtproto' AND bot_id IS NULL AND telegram_file_id IS NULL)
        OR (storage_kind = 'bot' AND bot_id IS NOT NULL AND telegram_file_id IS NOT NULL)
    );

ALTER TABLE /* TEMPLATE: schema */upload_parts
    ADD COLUMN storage_kind /* TEMPLATE: schema */part_storage_kind NOT NULL DEFAULT 'mtproto',
    ADD COLUMN bot_id BIGINT,
    ADD COLUMN telegram_file_id TEXT;

-- 上传中的 bot part 在 claim 时只有 bot_id、尚无 file_id，所以此处只要求 bot_id 存在；
-- file_id 的完整性由 stored 状态约束保证。
ALTER TABLE /* TEMPLATE: schema */upload_parts
    ADD CONSTRAINT upload_parts_bot_storage_check CHECK (
        (storage_kind = 'mtproto' AND bot_id IS NULL AND telegram_file_id IS NULL)
        OR (storage_kind = 'bot' AND bot_id IS NOT NULL)
    );

ALTER TABLE /* TEMPLATE: schema */upload_parts
    ADD CONSTRAINT upload_parts_bot_file_id_check CHECK (
        storage_kind <> 'bot' OR state <> 'stored' OR telegram_file_id IS NOT NULL
    );

-- +goose Down

ALTER TABLE /* TEMPLATE: schema */upload_parts
    DROP CONSTRAINT upload_parts_bot_file_id_check,
    DROP CONSTRAINT upload_parts_bot_storage_check,
    DROP COLUMN telegram_file_id,
    DROP COLUMN bot_id,
    DROP COLUMN storage_kind;

ALTER TABLE /* TEMPLATE: schema */file_parts
    DROP CONSTRAINT file_parts_bot_storage_check,
    DROP COLUMN telegram_file_id,
    DROP COLUMN bot_id,
    DROP COLUMN storage_kind;

DROP TYPE /* TEMPLATE: schema */part_storage_kind;
