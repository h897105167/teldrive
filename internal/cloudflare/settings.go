package cloudflare

// settings.go —— 设置端点用的 config.toml 局部字段持久化。
// 复用 Deployer.updateConfig 的「备份 → 重载 → marshal → 写回」流程，
// 保证设置页修改的字段落盘且重启后仍生效。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/config"
)

// SaveGatewayWorkerMinBytes 把下载分流阈值持久化到 config.toml 的
// [gateway].worker-min-bytes：先备份，再重载当前配置、只改这一个字段后
// 原子写回（临时文件 + rename）。任何失败都不会破坏现有配置文件。
func SaveGatewayWorkerMinBytes(configPath string, workerMinBytes int64) error {
	return saveGateway(configPath, func(cfg *config.Config) {
		cfg.Gateway.WorkerMinBytes = workerMinBytes
	})
}

// SaveGatewayEnabled 把数据面开关持久化到 config.toml 的 [gateway].enabled。
// 与 SaveGatewayWorkerMinBytes 共用同一套备份与原子写回流程。
func SaveGatewayEnabled(configPath string, enabled bool) error {
	return saveGateway(configPath, func(cfg *config.Config) {
		cfg.Gateway.Enabled = enabled
	})
}

// SaveGatewaySettings 一次性写回开关与阈值：切模式时如果同时改了阈值，
// 两次独立写回会各留一个备份文件、且中途失败会留下半更新状态，所以合并成一次。
func SaveGatewaySettings(configPath string, enabled *bool, workerMinBytes *int64) error {
	return saveGateway(configPath, func(cfg *config.Config) {
		if enabled != nil {
			cfg.Gateway.Enabled = *enabled
		}
		if workerMinBytes != nil {
			cfg.Gateway.WorkerMinBytes = *workerMinBytes
		}
	})
}

// configBackupKeep 是 config.toml 备份文件的保留数量。设置页的开关每次切换
// 都会写一个备份；不设上限时一天下来会在 /etc/teldrive 里堆出几十个副本，
// 掩盖真正需要人工回滚的那一个。
const configBackupKeep = 10

// pruneBackups 删除最旧的备份，只保留最近 keep 个。备份名以 UTC 时间戳结尾，
// 所以按名字排序即按时间排序。清理失败不影响主流程（写回已经成功）。
func pruneBackups(configPath string, keep int) {
	if keep <= 0 {
		return
	}
	dir := filepath.Dir(configPath)
	prefix := filepath.Base(configPath) + ".bak-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// saveGateway 读取现有配置、应用 mutate、再原子写回。备份文件名带 UTC 时间戳，
// 失败时不改动原文件。
func saveGateway(configPath string, mutate func(*config.Config)) error {
	if strings.TrimSpace(configPath) == "" {
		return fmt.Errorf("config path is empty")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	backup := configPath + ".bak-" + time.Now().UTC().Format("20060102150405")
	if err := os.WriteFile(backup, raw, 0o644); err != nil {
		return fmt.Errorf("write config backup: %w", err)
	}
	pruneBackups(configPath, configBackupKeep)

	cfg, err := config.LoadFromPath(configPath)
	if err != nil {
		return fmt.Errorf("parse current config from %q: %w", configPath, err)
	}
	mutate(&cfg)

	tomlBytes, err := marshalConfigTOML(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp := filepath.Join(filepath.Dir(configPath), "."+filepath.Base(configPath)+".tmp")
	if err := os.WriteFile(tmp, tomlBytes, 0o644); err != nil {
		return fmt.Errorf("write config temp file: %w", err)
	}
	if err := os.Rename(tmp, configPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
