package store

import (
	"testing"
	"time"
)

// DeliveryDelay 必须按时间戳自带的偏移解析：
// 进程时区是 UTC（POSIX TZ="CST-8" 下 Go 的实际行为）时，
// 带 "+08" 的北京时间戳曾因差值为负被钳成 0。
func TestDeliveryDelayOffsetAware(t *testing.T) {
	// 模拟 UTC 进程时区，复现设备上的实际情况
	t.Setenv("TZ", "UTC")
	time.Local = time.UTC

	tenAgo := time.Now().Add(-10 * time.Second)
	// 北京时间（+08）格式
	tsBJ := tenAgo.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02T15:04:05") + "+08"
	// UTC（+0000）格式
	tsUTC := tenAgo.UTC().Format("2006-01-02T15:04:05") + "+0000"

	for name, ts := range map[string]string{"+08": tsBJ, "+0000": tsUTC} {
		d := DeliveryDelay(ts)
		if d == nil {
			t.Fatalf("%s: 不应返回 nil", name)
		}
		if *d < 5 || *d > 20 {
			t.Errorf("%s: delay=%d, 期望约 10", name, *d)
		}
	}

	// 未来时间戳钳 0
	future := time.Now().Add(5 * time.Minute).UTC().Format("2006-01-02T15:04:05") + "+0000"
	if d := DeliveryDelay(future); d == nil || *d != 0 {
		t.Errorf("future: delay=%v, 期望 0", d)
	}
}
