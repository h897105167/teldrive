-- +goose Up

-- 迁移台账里 10 号位置的历史差异修复。
--
-- 本迁移最初以 000010 发布给过一次既有部署；随后上游把 000010 分配给了
-- user_preferences，合并后本迁移顺延到 000013。对于已按旧编号应用过的库，
-- 台账中 version_id = 10 已标记为已应用，但那一版的 DDL 是 part_storage_kind，
-- 而非 user_preferences —— goose 只比较版本号，因此会跳过 000010，
-- 使这类库缺少 user_preferences 表。
--
-- 全新库由 000010 正常建表，本迁移是空操作；既有库在这里补建。
CREATE TABLE IF NOT EXISTS /* TEMPLATE: schema */user_preferences (
    user_id BIGINT PRIMARY KEY REFERENCES /* TEMPLATE: schema */users(user_id) ON DELETE CASCADE,
    upload_encryption BOOLEAN NOT NULL DEFAULT false,
    upload_conflict_policy /* TEMPLATE: schema */name_conflict_policy NOT NULL DEFAULT 'rename',
    upload_concurrency INTEGER NOT NULL DEFAULT 3,
    upload_part_size BIGINT NOT NULL DEFAULT 536870912,
    file_sort TEXT NOT NULL DEFAULT 'name',
    file_order TEXT NOT NULL DEFAULT 'asc',
    theme TEXT NOT NULL DEFAULT 'system',
    -- NULL means the client detects the language from the browser. The API
    -- contract owns the list of supported locales.
    locale TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ,
    CONSTRAINT user_preferences_upload_concurrency_range CHECK (upload_concurrency BETWEEN 1 AND 12),
    CONSTRAINT user_preferences_upload_part_size_valid CHECK (
        upload_part_size BETWEEN 16777216 AND 2147483648
        AND upload_part_size % 16777216 = 0
    ),
    CONSTRAINT user_preferences_file_sort_valid CHECK (file_sort IN ('name', 'updatedAt', 'size')),
    CONSTRAINT user_preferences_file_order_valid CHECK (file_order IN ('asc', 'desc')),
    CONSTRAINT user_preferences_theme_valid CHECK (theme IN ('system', 'light', 'dark')),
    CONSTRAINT user_preferences_locale_length CHECK (locale IS NULL OR char_length(locale) BETWEEN 2 AND 16)
);

-- +goose Down

-- 回滚不动 user_preferences：它属于 000010，删表应由 000010 的 Down 负责。
SELECT 1;
