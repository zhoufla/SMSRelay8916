// Package store 负责短信归档库（SQLite）。
//
// 这里持久保存短信、发送队列、通道投递结果和 DDNS 运行态。
// 配置（通道、DDNS、启动参数）不在这里 —— 唯一来源是 UCI /etc/config/smsrelay。
//
// 旧库增量迁移；打不开时保留现场，不自动重建空库。
package store

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	// CGO 驱动：OpenWrt 包用 -tags libsqlite3 链系统的 libsqlite3（体积小、
	// 与系统其它程序共用同一份库）；宿主机上不带该 tag 时退化为内置
	// amalgamation，方便直接 go test。
	_ "github.com/mattn/go-sqlite3"

	"github.com/zhoufla/SMSRelay8916/internal/otp"
)

// Message 对应 messages 表的一行。
// JSON 字段名必须与既有 Web UI 保持一致，不要改。
type Message struct {
	ID           int64  `json:"id"`
	Dir          string `json:"dir"`
	Peer         string `json:"peer"`
	Body         string `json:"body"`
	Code         string `json:"code"` // 从正文提取的验证码，仅入站短信可能有
	TS           string `json:"ts"`
	State        string `json:"state"`
	Storage      string `json:"storage"`
	SMSC         string `json:"smsc"`
	PDUType      string `json:"pdu_type"`
	MMPath       string `json:"mm_path"`
	Hash         string `json:"hash"`
	Forwarded    int    `json:"forwarded"`
	Attempts     int    `json:"attempts"`
	ForwardError string `json:"forward_error"`
	DelayS       *int   `json:"delay_s"`
	CreatedAt    string `json:"created_at"`
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    dir           TEXT NOT NULL,
    peer          TEXT,
    body          TEXT,
    code          TEXT NOT NULL DEFAULT '',
    ts            TEXT,
    state         TEXT,
    storage       TEXT,
    smsc          TEXT,
    pdu_type      TEXT,
    mm_path       TEXT,
    hash          TEXT UNIQUE,
    forwarded     INTEGER DEFAULT 0,
    attempts      INTEGER DEFAULT 0,
    forward_error TEXT,
    delay_s       INTEGER,
    created_at    TEXT DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_ts ON messages(ts DESC);
CREATE INDEX IF NOT EXISTS idx_fw ON messages(dir, forwarded);

-- 每个通道各自的投递结果（一条短信 × 一个通道）。
-- 通道本身不在这里 —— 那是配置，唯一来源是 UCI /etc/config/smsrelay。
-- channel_id 由 UCI 段名稳定映射而来，配置改来改去也不会断历史。
CREATE TABLE IF NOT EXISTS channel_deliveries (
    message_id INTEGER NOT NULL,
    channel_id INTEGER NOT NULL,
    status     TEXT,
    attempts   INTEGER DEFAULT 0,
    last_error TEXT,
    updated_at TEXT DEFAULT (datetime('now','localtime')),
    PRIMARY KEY (message_id, channel_id)
);

-- DDNS 记录的同步结果（运行态，不是配置）。
-- 配置（域名/类型/token/开关）在 UCI 里；这里只记"上次解析到哪个 IP、有没有报错"，
-- 属于可丢数据，升级时随库一起清空也不影响服务。
CREATE TABLE IF NOT EXISTS ddns_state (
    id          INTEGER PRIMARY KEY,
    last_ip     TEXT,
    last_update TEXT,
    last_error  TEXT
);
`

// Store 是归档库句柄。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）归档库，并完成必要的 pragma 与列升级。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	// 参数放在 DSN 中，保证连接池里的每条连接都有相同的同步和等待策略。
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	q.Set("_busy_timeout", "15000")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// WAL 下读不阻塞写；连接数有界，允许本地 RPC 分页查询并发。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=15000",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(sendSchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{db: db}
	if err := st.backfillCodes(); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// migrate 给老库补列。Python 版早期连 delay_s 都没有。
func migrate(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(messages)")
	if err != nil {
		return fmt.Errorf("读取表结构失败: %w", err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("读取列信息失败: %w", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, add := range []struct{ name, ddl string }{
		{"delay_s", "ALTER TABLE messages ADD COLUMN delay_s INTEGER"},
		{"code", "ALTER TABLE messages ADD COLUMN code TEXT NOT NULL DEFAULT ''"},
	} {
		found := false
		for _, c := range cols {
			if c == add.name {
				found = true
				break
			}
		}
		if !found {
			if _, err := db.Exec(add.ddl); err != nil {
				return fmt.Errorf("添加 %s 列失败: %w", add.name, err)
			}
		}
	}
	return nil
}

// backfillCodes 给升级前的历史入站短信补提取验证码。
// 只处理 code 为空的入站行，第一次启动跑完后就再也没有待回填的行，开销趋近于零。
func (s *Store) backfillCodes() error {
	rows, err := s.db.Query("SELECT id, body FROM messages WHERE dir='in' AND code=''")
	if err != nil {
		return fmt.Errorf("回填验证码查询失败: %w", err)
	}
	type item struct {
		id   int64
		code string
	}
	var items []item
	for rows.Next() {
		var id int64
		var body sql.NullString
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return err
		}
		if c := otp.Extract(body.String); c != "" {
			items = append(items, item{id, c})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, it := range items {
		if _, err := s.db.Exec("UPDATE messages SET code=? WHERE id=?", it.code, it.id); err != nil {
			return err
		}
	}
	return nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// ContentHash 计算短信指纹。
//
// 身份只由内容决定，绝不能用 ModemManager 的对象路径：
// MM 会回收复用 SMS/3 这类编号，用路径判重会把新短信误判成"已存在"，
// 于是跳过入库却照样删除对象 —— 消息被静默销毁（踩过）。
func ContentHash(pduType, peer, ts, body string) string {
	h := sha1.Sum([]byte(pduType + "|" + peer + "|" + ts + "|" + body))
	return hex.EncodeToString(h[:])
}

// DeliveryDelay 返回 PDU 时间戳与本机时间的差（秒）。
// 大于 60 通常说明短信在短信中心排过队，即设备当时处于空闲态、寻呼没打到。
func DeliveryDelay(ts string) *int {
	if len(ts) < 19 {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", ts[:19], time.Local)
	if err != nil {
		return nil
	}
	d := int(time.Since(t).Seconds())
	if d < 0 {
		d = 0
	}
	return &d
}

// Archive 把一条短信写入归档库，返回是否已确认归档（true 才允许删除 modem 内的副本）。
//
// 三种情况：
//  1. 内容哈希已存在 —— 只补状态，视为已归档
//  2. 近期发送队列记录的路径、号码、正文同时匹配 —— 只补待确认状态
//  3. 否则插入新行
func (s *Store) Archive(m Message) (bool, error) {
	m.Hash = ContentHash(m.PDUType, m.Peer, m.TS, m.Body)
	// 验证码是正文的派生数据，在归档入口统一提取（只关心入站）。
	// 放在这里而不是调用方，保证插入与哈希命中两条路径行为一致。
	if m.Dir == "in" && m.Code == "" {
		m.Code = otp.Extract(m.Body)
	}

	var exists int
	err := s.db.QueryRow("SELECT 1 FROM messages WHERE hash=? LIMIT 1", m.Hash).Scan(&exists)
	if err == nil {
		// 已存在：把可能残留的临时状态补成终态
		_, uerr := s.db.Exec(
			"UPDATE messages SET state=? WHERE hash=? AND (state IS NULL OR state IN ('queued','sending','--'))",
			m.State, m.Hash)
		if uerr != nil {
			return false, uerr
		}
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}

	if m.Dir == "out" {
		// 发送队列已经持久化的占位记录，只补充同路径、同号码、同正文的状态。
		res, err := s.db.Exec("UPDATE messages SET state=? WHERE dir='out' AND mm_path=? AND peer=? AND body=? AND state IN ('queued','sending','unknown') AND created_at >= datetime('now','localtime','-5 minutes')", m.State, m.MMPath, m.Peer, m.Body)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return true, s.reconcileSendTasks(m.MMPath)
		}
	}

	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO messages
		   (dir, peer, body, ts, state, storage, smsc, pdu_type, mm_path, hash, delay_s, code)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Dir, m.Peer, m.Body, m.TS, m.State, m.Storage, m.SMSC, m.PDUType, m.MMPath, m.Hash, m.DelayS, m.Code)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateStateByPath 用于发送流程：把 queued 占位记录更新为最终状态。
func (s *Store) UpdateStateByPath(path, state string) error {
	_, err := s.db.Exec(
		"UPDATE messages SET state=? WHERE mm_path=? AND (state IS NULL OR state IN ('queued','--'))",
		state, path)
	return err
}

// FailByPath 把发送失败的记录标记为 failed。
func (s *Store) FailByPath(path, reason string) error {
	_, err := s.db.Exec(
		"UPDATE messages SET state='failed', forward_error=? WHERE mm_path=?",
		reason, path)
	return err
}

// StateByPath 读取某条记录当前的状态（发送流程用）。
func (s *Store) StateByPath(path string) (string, error) {
	var st sql.NullString
	err := s.db.QueryRow(
		"SELECT state FROM messages WHERE mm_path=? ORDER BY id DESC LIMIT 1", path).Scan(&st)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return st.String, nil
}

// Get 按 id 取一条。
func (s *Store) Get(id int64) (*Message, error) {
	row := s.db.QueryRow(selectAll+" FROM messages WHERE id=?", id)
	m, err := scanMessage(row)
	if err != nil {
		return nil, err
	}
	return m, nil
}

const selectAll = `SELECT id, dir, peer, body, code, ts, state, storage, smsc, pdu_type,
       mm_path, hash, forwarded, attempts, forward_error, delay_s, created_at`

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanMessage(r rowScanner) (*Message, error) {
	var (
		m         Message
		peer      sql.NullString
		body      sql.NullString
		ts        sql.NullString
		state     sql.NullString
		storage   sql.NullString
		smsc      sql.NullString
		pduType   sql.NullString
		mmPath    sql.NullString
		hash      sql.NullString
		fwErr     sql.NullString
		delay     sql.NullInt64
		createdAt sql.NullString
	)
	err := r.Scan(&m.ID, &m.Dir, &peer, &body, &m.Code, &ts, &state, &storage, &smsc,
		&pduType, &mmPath, &hash, &m.Forwarded, &m.Attempts, &fwErr, &delay, &createdAt)
	if err != nil {
		return nil, err
	}
	m.Peer, m.Body, m.TS, m.State = peer.String, body.String, ts.String, state.String
	m.Storage, m.SMSC, m.PDUType, m.MMPath = storage.String, smsc.String, pduType.String, mmPath.String
	m.Hash, m.ForwardError, m.CreatedAt = hash.String, fwErr.String, createdAt.String
	if delay.Valid {
		v := int(delay.Int64)
		m.DelayS = &v
	}
	return &m, nil
}

// ListQuery 是本地 RPC 短信分页查询条件。
type ListQuery struct {
	Dir     string
	Q       string
	HasCode bool // 只看提取到验证码的消息
	Limit   int
	Offset  int
}

// List 返回分页结果与总数。
func (s *Store) List(q ListQuery) ([]Message, int, error) {
	where := "WHERE 1=1"
	var args []interface{}
	if q.Dir == "in" || q.Dir == "out" {
		where += " AND dir=?"
		args = append(args, q.Dir)
	}
	if q.HasCode {
		where += " AND code != ''"
	}
	if q.Q != "" {
		where += " AND (peer LIKE ? OR body LIKE ?)"
		like := "%" + q.Q + "%"
		args = append(args, like, like)
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM messages "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(
		selectAll+" FROM messages "+where+" ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?",
		append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	return out, total, rows.Err()
}

// Delete 删除一条记录。
func (s *Store) Delete(id int64) error {
	_, err := s.db.Exec("DELETE FROM messages WHERE id=?", id)
	return err
}

// deletePlaceholder 生成 "?,?,?" 占位符。
func deletePlaceholder(n int) (string, []interface{}) {
	ph := strings.TrimSuffix(strings.Repeat("?,", n), ",")
	args := make([]interface{}, n)
	return ph, args
}

// DeleteByIDs 批量删除记录及其通道投递记录，返回实际删除的条数。
// 上限校验（防误传超长列表）由 API 层负责，这里只管执行。
func (s *Store) DeleteByIDs(ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	ph, args := deletePlaceholder(len(ids))
	for i, id := range ids {
		args[i] = id
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// 先删投递记录再删主表，顺序与 DeleteChannel 一致
	if _, err := tx.Exec("DELETE FROM channel_deliveries WHERE message_id IN ("+ph+")", args...); err != nil {
		return 0, err
	}
	res, err := tx.Exec("DELETE FROM messages WHERE id IN ("+ph+") AND COALESCE(state,'') NOT IN ('queued','sending')", args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := pruneSendTasks(tx); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// DeleteOlderThan 删除 ts 早于 cutoff（形如 2006-01-02）的记录及其通道投递记录。
//
// ts 带 +08 时区后缀，SQLite 的 date() 解析不了，所以用 substr(ts,1,10)
// 做字符串比较 —— 与 Today() 同一套路，不能改成 date(ts)。
// 删完顺手截断 WAL，避免大批删除后 -wal 文件只增不减（设备 eMMC 只有 3.3GB）。
func (s *Store) DeleteOlderThan(cutoff string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"DELETE FROM channel_deliveries WHERE message_id IN "+
			"(SELECT id FROM messages WHERE substr(ts,1,10) < ?)", cutoff); err != nil {
		return 0, err
	}
	res, err := tx.Exec("DELETE FROM messages WHERE substr(ts,1,10) < ? AND (dir='in' OR COALESCE(state,'') NOT IN ('queued','sending'))", cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := pruneSendTasks(tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// 截断是尽力而为，失败不影响删除结果
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return n, nil
}

// ResetForward 让某条记录重新进入转发队列。
func (s *Store) ResetForward(id int64) error {
	_, err := s.db.Exec(
		"UPDATE messages SET forwarded=0, attempts=0, forward_error=NULL WHERE id=?", id)
	return err
}

// PendingForward 取出待转发的入站短信。
func (s *Store) PendingForward(limit int) ([]Message, error) {
	rows, err := s.db.Query(
		selectAll+" FROM messages WHERE dir='in' AND forwarded=0 AND attempts<3 ORDER BY id LIMIT ?",
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// MarkForwarded 标记转发成功。
func (s *Store) MarkForwarded(id int64) error {
	_, err := s.db.Exec(
		"UPDATE messages SET forwarded=1, attempts=attempts+1, forward_error=NULL WHERE id=?", id)
	return err
}

// MarkForwardFailed 记录一次转发失败；attempts>=3 时置 forwarded=2（不再重试）。
func (s *Store) MarkForwardFailed(id int64, reason string) error {
	_, err := s.db.Exec(
		"UPDATE messages SET attempts=attempts+1, forward_error=?, "+
			"forwarded=CASE WHEN attempts>=2 THEN 2 ELSE 0 END WHERE id=?",
		reason, id)
	return err
}

// Counts 是汇总统计。
type Counts struct {
	Inbox   int `json:"inbox"`
	Sent    int `json:"sent"`
	Total   int `json:"total"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

// Counts 返回累计统计。
func (s *Store) Counts() (Counts, error) {
	var c Counts
	var inbox, sent, pending, failed sql.NullInt64
	var total int
	// 空表时 SUM() 返回 NULL，COUNT(*) 返回 0，所以前三个用 NullInt64 接。
	err := s.db.QueryRow(
		"SELECT SUM(dir='in'), SUM(dir='out'), COUNT(*), SUM(dir='in' AND forwarded=0), SUM(dir='in' AND forwarded=2) FROM messages").
		Scan(&inbox, &sent, &total, &pending, &failed)
	if err != nil {
		return c, err
	}
	c.Inbox, c.Sent, c.Total = int(inbox.Int64), int(sent.Int64), total
	c.Pending = int(pending.Int64)
	c.Failed = int(failed.Int64)
	return c, nil
}

// Today 是当日收发统计。
type Today struct {
	Inbox int `json:"inbox"`
	Sent  int `json:"sent"`
}

// Today 返回当日统计。注意 ts 形如 2026-10-31T20:15:16+08，
// SQLite 的 date() 解析不了带 +08 的时区后缀，所以用字符串截取比较。
func (s *Store) Today() (Today, error) {
	var t Today
	var inbox, sent sql.NullInt64
	today := time.Now().Format("2006-01-02")
	err := s.db.QueryRow(
		"SELECT SUM(dir='in'), SUM(dir='out') FROM messages WHERE substr(ts,1,10)=?",
		today).Scan(&inbox, &sent)
	if err != nil {
		return t, err
	}
	t.Inbox, t.Sent = int(inbox.Int64), int(sent.Int64)
	return t, nil
}

// LastInbound 返回最近一条入站短信的时间戳。
func (s *Store) LastInbound() string {
	var ts sql.NullString
	err := s.db.QueryRow(
		"SELECT ts FROM messages WHERE dir='in' ORDER BY id DESC LIMIT 1").Scan(&ts)
	if err != nil {
		return ""
	}
	return ts.String
}

// Paging 是 24 小时寻呼质量统计。
type Paging struct {
	Count     int `json:"count"`
	MaxDelayS int `json:"max_delay_s"`
	SlowCount int `json:"slow_count"`
}

// Paging24h 统计 24 小时内的投递延迟，用于判断保活是否足够。
func (s *Store) Paging24h() (Paging, error) {
	var p Paging
	var n, mx, slow sql.NullInt64
	err := s.db.QueryRow(
		"SELECT COUNT(*), IFNULL(MAX(delay_s),0), IFNULL(SUM(delay_s>=60),0) FROM messages "+
			"WHERE dir='in' AND delay_s IS NOT NULL "+
			"AND created_at >= datetime('now','localtime','-24 hours')").
		Scan(&n, &mx, &slow)
	if err != nil {
		return p, err
	}
	p.Count, p.MaxDelayS, p.SlowCount = int(n.Int64), int(mx.Int64), int(slow.Int64)
	return p, nil
}

// ---------------------------------------------------------------------------
// DDNS 运行态
// ---------------------------------------------------------------------------

// DDNSState 是某条 DDNS 记录最近一次同步的结果。
// 配置（域名/类型/token/开关）在 UCI 里；这里只存"上次解析到哪个 IP、
// 有没有报错"，属于可丢数据，升级时随库清空也不影响服务。
type DDNSState struct {
	LastIP     string
	LastUpdate string
	LastError  string
}

// GetDDNSState 读取一条记录的状态。没有记录时返回零值，不算错误。
func (s *Store) GetDDNSState(id int64) (DDNSState, error) {
	var st DDNSState
	var ip, upd, errMsg sql.NullString
	err := s.db.QueryRow(
		"SELECT last_ip, last_update, last_error FROM ddns_state WHERE id=?", id).
		Scan(&ip, &upd, &errMsg)
	if err == sql.ErrNoRows {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.LastIP, st.LastUpdate, st.LastError = ip.String, upd.String, errMsg.String
	return st, nil
}

// SetDDNSState 记录一次同步结果。ip 为空表示这次没拿到地址（只更新错误与时间）。
// 成功后 last_error 清空；写空字符串等价于"恢复正常"。
func (s *Store) SetDDNSState(id int64, ip, errMsg string) error {
	_, err := s.db.Exec(
		`INSERT INTO ddns_state (id, last_ip, last_update, last_error)
		 VALUES (?,?,datetime('now','localtime'),?)
		 ON CONFLICT(id) DO UPDATE SET
		   last_ip=COALESCE(NULLIF(excluded.last_ip,''), ddns_state.last_ip),
		   last_error=NULLIF(excluded.last_error,''),
		   last_update=excluded.last_update`,
		id, ip, errMsg)
	return err
}

// ---------------------------------------------------------------------------
// 通道投递记录
// ---------------------------------------------------------------------------

// DeliveryStatus 返回某条短信在某个通道上的投递状态（空表示还没投过）。
func (s *Store) DeliveryStatus(messageID, channelID int64) string {
	var st sql.NullString
	err := s.db.QueryRow(
		"SELECT status FROM channel_deliveries WHERE message_id=? AND channel_id=?",
		messageID, channelID).Scan(&st)
	if err != nil {
		return ""
	}
	return st.String
}

// MarkDelivery 记录一次投递结果。
func (s *Store) MarkDelivery(messageID, channelID int64, status, errMsg string) error {
	_, err := s.db.Exec(
		`INSERT INTO channel_deliveries (message_id, channel_id, status, attempts, last_error, updated_at)
		 SELECT ?,?,?,1,?,datetime('now','localtime') WHERE EXISTS (SELECT 1 FROM messages WHERE id=?)
		 ON CONFLICT(message_id, channel_id) DO UPDATE SET
		   status=excluded.status,
		   attempts=attempts+1,
		   last_error=excluded.last_error,
		   updated_at=excluded.updated_at`,
		messageID, channelID, status, errMsg, messageID)
	return err
}

// ResetDeliveries 清空某条短信的所有通道投递记录，让它重新入队。
func (s *Store) ResetDeliveries(messageID int64) error {
	_, err := s.db.Exec("DELETE FROM channel_deliveries WHERE message_id=?", messageID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		"UPDATE messages SET forwarded=0, attempts=0, forward_error=NULL WHERE id=?", messageID)
	return err
}

// PruneDeliveries 删掉已经不在配置里的通道所对应的投递记录。
//
// 通道从 UCI 里删掉后，库里的投递行就成了孤儿：不影响功能，但会让"某条短信
// 是否已全部转发"的判断永远为假。启动时调一次即可，成本只有一次全表扫描。
func (s *Store) PruneDeliveries(alive []int64) (int64, error) {
	q := "DELETE FROM channel_deliveries"
	var args []interface{}
	if len(alive) > 0 {
		q += " WHERE channel_id NOT IN (" + placeholders(len(alive)) + ")"
		for _, id := range alive {
			args = append(args, id)
		}
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// placeholders 生成 "?,?,?" 形式的占位符。
func placeholders(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	return b.String()
}
