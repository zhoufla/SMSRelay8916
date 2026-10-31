package logbuf

import (
	"fmt"
	"log"
	"strings"
	"testing"
)

// Write 要能解析标准库 log 的 LstdFlags 前缀，并按 WARN 子串判级。
func TestWriteParsesStdLog(t *testing.T) {
	b := New(10)
	// 走真正的 log 管道写进来，前缀格式才与线上完全一致
	logger := log.New(b, "", log.LstdFlags)
	logger.Printf("in  10086 2026-10-31T20:15:16+08: hello")
	logger.Printf("WARN 读取 / SMS/3 失败: timeout")

	all := b.Snapshot(0, "")
	if len(all) != 2 {
		t.Fatalf("应有 2 条, got %d", len(all))
	}
	if all[0].Level != "info" || all[1].Level != "warn" {
		t.Fatalf("级别判定不对: %q / %q", all[0].Level, all[1].Level)
	}
	for i, want := range []string{"in  10086", "WARN 读取"} {
		if !strings.HasPrefix(all[i].Msg, want) {
			t.Fatalf("第 %d 条正文应保留原样: %q", i, all[i].Msg)
		}
		if all[i].TS == "" || strings.Contains(all[i].TS, "/") {
			t.Fatalf("时间应转成 ISO 样式且非空: %q", all[i].TS)
		}
	}
}

// 没有 LstdFlags 前缀的输入按原文保存，不能丢。
func TestWriteWithoutPrefix(t *testing.T) {
	b := New(10)
	if _, err := b.Write([]byte("裸日志一行\n")); err != nil {
		t.Fatal(err)
	}
	all := b.Snapshot(0, "")
	if len(all) != 1 || all[0].Msg != "裸日志一行" {
		t.Fatalf("裸行保存不对: %+v", all)
	}
	if all[0].TS == "" {
		t.Fatal("裸行也要有兜底时间")
	}
}

// 超容量后淘汰最旧的（这就是日志功能的「周期清理」——天然免费）。
func TestRingEviction(t *testing.T) {
	b := New(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(b, "2026/10/31 20:15:0%d 第%d条\n", i, i)
	}
	all := b.Snapshot(0, "")
	if len(all) != 3 {
		t.Fatalf("容量应为 3, got %d", len(all))
	}
	if all[0].Msg != "第3条" || all[2].Msg != "第5条" {
		t.Fatalf("应淘汰最旧两条: %+v", all)
	}
}

// Snapshot 的 limit 与 warn 过滤。
func TestSnapshotLimitAndLevel(t *testing.T) {
	b := New(10)
	fmt.Fprint(b, "2026/10/31 20:15:01 a\n")
	fmt.Fprint(b, "2026/10/31 20:15:02 WARN b\n")
	fmt.Fprint(b, "2026/10/31 20:15:03 c\n")

	if got := b.Snapshot(2, ""); len(got) != 2 || got[0].Msg != "WARN b" || got[1].Msg != "c" {
		t.Fatalf("limit=2 应取末尾两条: %+v", got)
	}
	warns := b.Snapshot(0, "warn")
	if len(warns) != 1 || warns[0].Level != "warn" {
		t.Fatalf("warn 过滤应只剩 1 条: %+v", warns)
	}
}
