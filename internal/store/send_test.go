package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSendQueueRecovery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.QueueSend("request-1234567890", "10086", "hello", []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.QueueSend("request-1234567890", "10086", "hello", []string{"hello"})
	if err != nil || duplicate != id {
		t.Fatalf("duplicate: %d %v", duplicate, err)
	}
	if _, err := s.QueueSend("request-1234567890", "10010", "other", []string{"other"}); err == nil {
		t.Fatal("must reject reused ID with changed content")
	}
	queued, err := s.SendTask(id)
	if err != nil || len(queued.Parts) != 1 {
		t.Fatalf("task: %+v %v", queued, err)
	}
	if n, err := s.DeleteByIDs([]int64{queued.Parts[0].ID}); err != nil || n != 0 {
		t.Fatal("queued SMS must not be deleted")
	}
	task, err := s.ClaimSendTask()
	if err != nil || task.ID != id {
		t.Fatalf("claim: %+v %v", task, err)
	}
	if err := s.RecoverSendTasks(); err != nil {
		t.Fatal(err)
	}
	task, err = s.SendTask(id)
	if err != nil || task.State != "unknown" || task.Parts[0].State != "unknown" {
		t.Fatalf("recovery: %+v %v", task, err)
	}
	if next, err := s.ClaimSendTask(); err != nil || next != nil {
		t.Fatal("must not automatically resend an uncertain task")
	}
	if n, err := s.DeleteByIDs([]int64{task.Parts[0].ID}); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if _, err := s.SendTask(id); err == nil {
		t.Fatal("deleted message task must not retain plaintext")
	}
}

func TestReconcileUncertainSend(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.QueueSend("request-reconcile", "10086", "hello", []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.ClaimSendTask()
	if err != nil {
		t.Fatal(err)
	}
	path := "/org/freedesktop/ModemManager1/SMS/1"
	if err := s.UpdateSendPart(task.Parts[0].ID, path, "sending", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverSendTasks(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Archive(Message{Dir: "out", Peer: "10086", Body: "hello", PDUType: "submit", State: "sent", MMPath: path, TS: time.Now().Format(time.RFC3339)}); err != nil || !ok {
		t.Fatalf("archive: %v %v", ok, err)
	}
	task, err = s.SendTask(id)
	if err != nil || task.State != "sent" || task.Parts[0].State != "sent" {
		t.Fatalf("reconcile: %+v %v", task, err)
	}
	if _, total, err := s.List(ListQuery{Dir: "out", Limit: 30}); err != nil || total != 1 {
		t.Fatal("reconcile created duplicate history")
	}
}

func TestLateDeliveryDoesNotCreateOrphan(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.MarkDelivery(999, 1, "sent", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM channel_deliveries").Scan(&n); err != nil || n != 0 {
		t.Fatal("late delivery created orphan")
	}
}

func TestSnapshotIncludesUncheckpointedWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sms.db")
	snapshot := filepath.Join(dir, "upgrade.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Archive(Message{Dir: "in", Peer: "10086", Body: "验证码 123456", TS: "2026-11-11T12:00:00+0800", State: "received", PDUType: "deliver"}); err != nil {
		t.Fatal(err)
	}
	if err := BackupFile(path, snapshot); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored", "sms.db")
	if err := RestoreFile(snapshot, restored); err != nil {
		t.Fatal(err)
	}
	r, err := Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	items, total, err := r.List(ListQuery{Dir: "in", Limit: 30})
	if err != nil || total != 1 || items[0].Code != "123456" {
		t.Fatalf("restore: %+v %d %v", items, total, err)
	}
	st, err := os.Stat(restored)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("database permissions must be private")
	}
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatal("consumed snapshot must not be replayed at next boot")
	}
}

func TestConnectionPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conns := []*sql.Conn{}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		c, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var synchronous, busy int
		if err := c.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if synchronous != 2 || busy != 15000 {
			t.Fatalf("synchronous=%d busy=%d", synchronous, busy)
		}
	}
}
