package store

import (
	"path/filepath"
	"testing"
)

// testChannelID 模拟一个由 UCI 段名映射出来的通道主键。
// 通道配置本身现在只存在于 /etc/config/smsrelay，库里只有投递记录。
const testChannelID int64 = 1

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 验证码在归档入口提取：入站提取、出站不提取。
func TestArchiveExtractsCode(t *testing.T) {
	st := openTestStore(t)

	in := Message{Dir: "in", Peer: "10086", Body: "【移动】验证码：483921，请勿泄露", TS: "2026-10-31T20:15:16+08", PDUType: "deliver"}
	ok, err := st.Archive(in)
	if err != nil || !ok {
		t.Fatalf("归档入站失败: ok=%v err=%v", ok, err)
	}
	m, err := st.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Code != "483921" {
		t.Fatalf("入站验证码 = %q, want 483921", m.Code)
	}

	out := Message{Dir: "out", Peer: "10086", Body: "余额还有1234元", TS: "2026-10-31T20:16:00+08", PDUType: "submit"}
	if _, err := st.Archive(out); err != nil {
		t.Fatal(err)
	}
	m, _ = st.Get(2)
	if m.Code != "" {
		t.Fatalf("出站不应提取验证码, got %q", m.Code)
	}
}

// 模拟老库升级：历史行没有 code，重开库后回填生效。
func TestBackfillCodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	in := Message{Dir: "in", Peer: "10690", Body: "您的验证码是 668822，10 分钟内有效", TS: "2026-10-30T09:00:00+08", PDUType: "deliver"}
	if _, err := st.Archive(in); err != nil {
		t.Fatal(err)
	}
	// 抹掉 code，模拟这一列出现之前就入库的行
	if _, err := st.db.Exec("UPDATE messages SET code=''"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	m, err := st2.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Code != "668822" {
		t.Fatalf("回填后验证码 = %q, want 668822", m.Code)
	}
}

// List 的 HasCode 过滤。
func TestListHasCode(t *testing.T) {
	st := openTestStore(t)
	for _, m := range []Message{
		{Dir: "in", Peer: "10086", Body: "验证码 556677", TS: "2026-10-31T20:15:16+08", PDUType: "deliver"},
		{Dir: "in", Peer: "10086", Body: "本月账单已出", TS: "2026-10-31T20:16:16+08", PDUType: "deliver"},
	} {
		if _, err := st.Archive(m); err != nil {
			t.Fatal(err)
		}
	}
	items, total, err := st.List(ListQuery{HasCode: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].Code != "556677" {
		t.Fatalf("HasCode 过滤结果 = %d 条 %+v, want 1 条验证码 556677", total, items)
	}
}

// 批量删除要连带清掉 channel_deliveries。
func TestDeleteByIDs(t *testing.T) {
	st := openTestStore(t)
	for _, m := range []Message{
		{Dir: "in", Peer: "10086", Body: "第一条", TS: "2026-10-31T20:15:16+08", PDUType: "deliver"},
		{Dir: "in", Peer: "10086", Body: "第二条", TS: "2026-10-31T20:16:16+08", PDUType: "deliver"},
		{Dir: "in", Peer: "10086", Body: "第三条", TS: "2026-10-31T20:17:16+08", PDUType: "deliver"},
	} {
		if _, err := st.Archive(m); err != nil {
			t.Fatal(err)
		}
	}
	chID := testChannelID
	for _, id := range []int64{1, 2, 3} {
		if err := st.MarkDelivery(id, chID, "sent", ""); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.DeleteByIDs([]int64{1, 3})
	if err != nil || n != 2 {
		t.Fatalf("DeleteByIDs = %d, %v; want 2 条", n, err)
	}
	if _, err := st.Get(1); err == nil {
		t.Fatal("id=1 应已被删除")
	}
	if _, err := st.Get(2); err != nil {
		t.Fatalf("id=2 不应被删: %v", err)
	}
	if got := st.DeliveryStatus(1, chID); got != "" {
		t.Fatalf("id=1 的投递记录应连带删除, got %q", got)
	}
	if got := st.DeliveryStatus(2, chID); got != "sent" {
		t.Fatalf("id=2 的投递记录应保留, got %q", got)
	}
}

// 按天清理：早于 cutoff 的删掉，之后的保留，投递记录连带清理。
func TestDeleteOlderThan(t *testing.T) {
	st := openTestStore(t)
	chID := testChannelID
	for _, m := range []Message{
		{Dir: "in", Peer: "10086", Body: "旧短信", TS: "2020-01-01T10:00:00+08", PDUType: "deliver"},
		{Dir: "in", Peer: "10086", Body: "新短信", TS: "2026-10-31T20:15:16+08", PDUType: "deliver"},
	} {
		if _, err := st.Archive(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkDelivery(1, chID, "sent", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDelivery(2, chID, "sent", ""); err != nil {
		t.Fatal(err)
	}

	n, err := st.DeleteOlderThan("2024-01-01")
	if err != nil || n != 1 {
		t.Fatalf("DeleteOlderThan = %d, %v; want 1 条", n, err)
	}
	if _, err := st.Get(1); err == nil {
		t.Fatal("2020 年的记录应已被删除")
	}
	if _, err := st.Get(2); err != nil {
		t.Fatalf("2026 年的记录不应被删: %v", err)
	}
	if got := st.DeliveryStatus(1, chID); got != "" {
		t.Fatalf("旧记录的投递记录应连带删除, got %q", got)
	}
	if got := st.DeliveryStatus(2, chID); got != "sent" {
		t.Fatalf("新记录的投递记录应保留, got %q", got)
	}
}
