// Package logbuf 维护一个有界的内存日志环形缓冲。
//
// 接在标准库 log 输出后面，stderr 照常进入 OpenWrt logd，
// 缓冲里的副本供原生 LuCI 查看，容量固定、写满自动淘汰最旧的。
// 是否跨重启保存系统日志由 logd 配置决定。
package logbuf

import (
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Entry 是缓冲里的一条日志。
type Entry struct {
	TS    string `json:"ts"`
	Level string `json:"level"` // info / warn
	Msg   string `json:"msg"`
}

// stdPrefixLen 是 log.LstdFlags 输出的时间前缀长度（含尾随空格）：
//
//	2006/01/02 15:04:05 message...
const stdPrefixLen = 20

// Buffer 是固定容量的日志环形缓冲，Write 使其实现 io.Writer。
type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	max     int
}

// New 创建容量 max 条的缓冲。
func New(max int) *Buffer {
	if max < 1 {
		max = 1
	}
	return &Buffer{max: max}
}

// Write 解析标准库 log 写来的一行并追加进缓冲，实现 io.Writer。
//
// 项目里所有警告日志都遵守「正文带 WARN 子串」的约定（如 "WARN 读取 ... 失败"），
// 据此判级；没有 LstdFlags 前缀的行（理论上只有测试或误接入）按原文保存。
func (b *Buffer) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line == "" {
		return len(p), nil
	}
	e := Entry{TS: time.Now().Format("2006-01-02 15:04:05"), Level: "info", Msg: line}
	if len(line) > stdPrefixLen {
		if t, err := time.Parse("2006/01/02 15:04:05", line[:stdPrefixLen-1]); err == nil {
			e.TS = t.Format("2006-01-02 15:04:05")
			e.Msg = line[stdPrefixLen:]
		}
	}
	if strings.Contains(e.Msg, "ERROR") {
		e.Level = "error"
	} else if strings.Contains(e.Msg, "WARN") {
		e.Level = "warn"
	}

	b.mu.Lock()
	b.entries = append(b.entries, e)
	if over := len(b.entries) - b.max; over > 0 {
		b.entries = b.entries[over:]
	}
	b.mu.Unlock()
	return len(p), nil
}

// Snapshot 返回最近 limit 条日志，时间正序（旧→新，与 journalctl 阅读习惯一致）。
// level 为 "warn" 时只返回警告；limit <= 0 表示不限制。
func (b *Buffer) Snapshot(limit int, level string) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()

	src := b.entries
	if level != "" {
		filtered := make([]Entry, 0, len(src))
		for _, e := range src {
			if e.Level == level {
				filtered = append(filtered, e)
			}
		}
		src = filtered
	}
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]Entry, limit)
	copy(out, src[len(src)-limit:])
	return out
}

type redactingWriter struct {
	out     io.Writer
	secrets []string
}

var logURL = regexp.MustCompile(`https?://[^\s"<>]+`)
var logNumber = regexp.MustCompile(`[0-9]{11,20}`)

func RedactingWriter(out io.Writer, secrets []string) io.Writer {
	return &redactingWriter{out: out, secrets: append([]string(nil), secrets...)}
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	s := string(p)
	for _, secret := range w.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	s = logURL.ReplaceAllString(s, "[URL]")
	s = logNumber.ReplaceAllString(s, "[number]")
	_, err := io.WriteString(w.out, s)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
