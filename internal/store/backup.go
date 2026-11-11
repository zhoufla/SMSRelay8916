package store

import (
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

const UpgradeSnapshot = "/etc/smsrelay/upgrade.db"

// BackupFile 使用 SQLite 在线快照，绝不直接复制正在写入的 .db/-wal。
func BackupFile(source, destination string) error {
	if source == destination {
		return fmt.Errorf("数据库路径不能使用升级快照保留路径")
	}
	u := url.URL{Scheme: "file", Path: source}
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_busy_timeout", "15000")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".snapshot-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	if _, err := db.Exec("VACUUM INTO ?", tmp); err != nil {
		return fmt.Errorf("创建数据库快照: %w", err)
	}
	f, err = os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmp, destination)
}

func RestoreFile(snapshot, destination string) error {
	u := url.URL{Scheme: "file", Path: snapshot}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return err
	}
	var check string
	err = db.QueryRow("PRAGMA quick_check").Scan(&check)
	db.Close()
	if err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("快照校验失败: %s", check)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	src, err := os.Open(snapshot)
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.CreateTemp(filepath.Dir(destination), ".restore-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = io.Copy(f, src); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// 本方法只由首次启动的 uci-defaults 在短信服务启动前执行。
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(destination + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	return os.Remove(snapshot)
}
