package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoufla/SMSRelay8916/internal/config"
	"github.com/zhoufla/SMSRelay8916/internal/store"
)

func validDBPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n\t ") || path == store.UpgradeSnapshot {
		return false
	}
	for _, prefix := range []string{"/tmp", "/var", "/proc", "/sys", "/dev"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	return true
}

func databaseCommand(command string) {
	cfg, err := config.Load(config.DefaultPath)
	if err == nil && !validDBPath(cfg.DBPath) {
		err = fmt.Errorf("数据库必须位于持久存储的绝对路径，不能使用升级快照路径")
	}
	if err == nil {
		if command == "backup" {
			err = store.BackupFile(cfg.DBPath, store.UpgradeSnapshot)
		} else {
			err = store.RestoreFile(store.UpgradeSnapshot, cfg.DBPath)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
