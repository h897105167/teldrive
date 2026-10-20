package cloudflare

// settings.go —— 设置端点用的 config.toml 局部字段持久化。
// 复用 Deployer.updateConfig 的「备份 → 重载 → marshal → 写回」流程，
// 保证设置页修改的字段落盘且重启后仍生效。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/config"
)

// SaveGatewayWorkerMinBytes 把下载分流阈值持久化到 config.toml 的
// [gateway].worker-min-bytes：先备份，再重载当前配置、只改这一个字段后
// 原子写回（临时文件 + rename）。任何失败都不会破坏现有配置文件。
func SaveGatewayWorkerMinBytes(configPath string, workerMinBytes int64) error {
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

	cfg, err := config.LoadFromPath(configPath)
	if err != nil {
		return fmt.Errorf("parse current config from %q: %w", configPath, err)
	}
	cfg.Gateway.WorkerMinBytes = workerMinBytes

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
