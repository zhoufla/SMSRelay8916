package store

import (
	"database/sql"
	"fmt"
	"time"
)

const sendSchema = `
CREATE TABLE IF NOT EXISTS send_tasks (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 request_id TEXT NOT NULL UNIQUE,
 request_hash TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'queued',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE TABLE IF NOT EXISTS send_parts (
 task_id INTEGER NOT NULL,
 ordinal INTEGER NOT NULL,
 message_id INTEGER NOT NULL,
 PRIMARY KEY(task_id, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_send_state ON send_tasks(state, id);
`

type SendTask struct {
	ID    int64     `json:"id"`
	State string    `json:"state"`
	Error string    `json:"error"`
	Parts []Message `json:"parts"`
}

// QueueSend 在同一事务内持久化请求及所有分段；重复 request_id 不会再次发送。
func (s *Store) QueueSend(requestID, peer, body string, parts []string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	var oldHash string
	hash := ContentHash("send", peer, "", body)
	err = tx.QueryRow("SELECT id,request_hash FROM send_tasks WHERE request_id=?", requestID).Scan(&id, &oldHash)
	if err == nil {
		if oldHash != hash {
			return 0, fmt.Errorf("请求标识已用于其他短信")
		}
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	var pending int
	if err := tx.QueryRow("SELECT COUNT(*) FROM send_tasks WHERE state IN ('queued','sending')").Scan(&pending); err != nil {
		return 0, err
	}
	if pending >= 100 {
		return 0, fmt.Errorf("发送队列已满")
	}
	res, err := tx.Exec("INSERT INTO send_tasks(request_id,request_hash) VALUES(?,?)", requestID, hash)
	if err != nil {
		return 0, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	ts := time.Now().Format("2006-01-02T15:04:05-0700")
	for i, part := range parts {
		res, err := tx.Exec("INSERT INTO messages(dir,peer,body,ts,state,pdu_type,hash) VALUES('out',?,?,?,'queued','submit',?)", peer, part, ts, fmt.Sprintf("send:%d:%d", id, i))
		if err != nil {
			return 0, err
		}
		mid, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec("INSERT INTO send_parts VALUES(?,?,?)", id, i, mid); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// RecoverSendTasks 不盲目重发已开始的请求：断电时运营商可能已经接受短信。
func (s *Store) RecoverSendTasks() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE messages SET state='unknown',forward_error='进程重启，发送结果待确认，请勿直接重发' WHERE id IN (SELECT p.message_id FROM send_parts p JOIN send_tasks t ON t.id=p.task_id WHERE t.state='sending') AND state IN ('queued','sending')"); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE send_tasks SET state='unknown',error='进程重启，发送结果待确认' WHERE state='sending'"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SendTask(id int64) (*SendTask, error) {
	t := &SendTask{ID: id, Parts: []Message{}}
	if err := s.db.QueryRow("SELECT state,error FROM send_tasks WHERE id=?", id).Scan(&t.State, &t.Error); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(selectAll+" FROM messages WHERE id IN (SELECT message_id FROM send_parts WHERE task_id=?) ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		t.Parts = append(t.Parts, *m)
	}
	return t, rows.Err()
}

func (s *Store) ClaimSendTask() (*SendTask, error) {
	var id int64
	err := s.db.QueryRow("SELECT id FROM send_tasks WHERE state='queued' ORDER BY id LIMIT 1").Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec("UPDATE send_tasks SET state='sending' WHERE id=? AND state='queued'", id); err != nil {
		return nil, err
	}
	return s.SendTask(id)
}

func (s *Store) UpdateSendPart(id int64, path, state, reason string) error {
	_, err := s.db.Exec("UPDATE messages SET mm_path=?,state=?,forward_error=? WHERE id=?", path, state, reason, id)
	return err
}

func (s *Store) FinishSendTask(id int64, state, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if state == "unknown" || state == "failed" {
		if _, err := tx.Exec("UPDATE messages SET state=?,forward_error=? WHERE state IN ('queued','sending') AND id IN (SELECT message_id FROM send_parts WHERE task_id=?)", state, reason, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE send_tasks SET state=?,error=? WHERE id=?", state, reason, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Checkpoint() error {
	var busy, log, checkpointed int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("数据库仍忙，无法完成一致性检查点")
	}
	return nil
}

func pruneSendTasks(tx *sql.Tx) error {
	if _, err := tx.Exec("DELETE FROM send_parts WHERE message_id NOT IN (SELECT id FROM messages)"); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM send_tasks WHERE state NOT IN ('queued','sending') AND id NOT IN (SELECT task_id FROM send_parts)")
	return err
}

func (s *Store) reconcileSendTasks(path string) error {
	_, err := s.db.Exec(`UPDATE send_tasks SET
 state=CASE WHEN EXISTS(SELECT 1 FROM send_parts p JOIN messages m ON m.id=p.message_id WHERE p.task_id=send_tasks.id AND m.state='failed') THEN 'failed' ELSE 'sent' END,
 error=''
 WHERE state='unknown' AND id IN (SELECT p.task_id FROM send_parts p JOIN messages m ON m.id=p.message_id WHERE m.mm_path=?)
 AND NOT EXISTS(SELECT 1 FROM send_parts p JOIN messages m ON m.id=p.message_id WHERE p.task_id=send_tasks.id AND m.state NOT IN ('sent','delivered','failed'))`, path)
	return err
}
