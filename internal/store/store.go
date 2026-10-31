// Package store 负责短信归档库（SQLite）。
//
// 表结构与早期 Python 版完全一致，因此可以直接复用同一个 sms.db，
// 不需要任何数据迁移。唯一的升级逻辑是给老库补 delay_s 列。
package store

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zhoufla/SMSRelay8916/internal/ddns"
	"github.com/zhoufla/SMSRelay8916/internal/forward"
)

// Message 对应 messages 表的一行。
// JSON 字段名必须与既有 Web UI 保持一致，不要改。
type Message struct {
	ID           int64  `json:"id"`
	Dir          string `json:"dir"`
	Peer         string `json:"peer"`
	Body         string `json:"body"`
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

-- 运行时设置（键值）。与 config.json 的分工：
--   config.json 只放启动必需项（listen/port/db_path）
--   settings 表放可在界面上改的项（own_number 等），改完立即生效
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT DEFAULT (datetime('now','localtime'))
);

-- 转发通道（钉钉 / 通用 webhook）
CREATE TABLE IF NOT EXISTS channels (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL,
    enabled    INTEGER DEFAULT 1,
    config     TEXT NOT NULL,
    created_at TEXT DEFAULT (datetime('now','localtime'))
);

-- 每个通道各自的投递结果（一条短信 × 一个通道）
CREATE TABLE IF NOT EXISTS channel_deliveries (
    message_id INTEGER NOT NULL,
    channel_id INTEGER NOT NULL,
    status     TEXT,
    attempts   INTEGER DEFAULT 0,
    last_error TEXT,
    updated_at TEXT DEFAULT (datetime('now','localtime')),
    PRIMARY KEY (message_id, channel_id)
);

-- DDNS 记录（一条记录 = 一个域名 + 一种记录类型，A/AAAA 自由组合）
CREATE TABLE IF NOT EXISTS ddns (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    zone_id     TEXT,
    api_token   TEXT NOT NULL,
    source      TEXT NOT NULL,
    proxied     INTEGER DEFAULT 0,
    ttl         INTEGER DEFAULT 1,
    enabled     INTEGER DEFAULT 1,
    last_ip     TEXT,
    last_update TEXT,
    last_error  TEXT,
    created_at  TEXT DEFAULT (datetime('now','localtime'))
);
`

// Store 是归档库句柄。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）归档库，并完成必要的 pragma 与列升级。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// WAL 下读不阻塞写；连接数不需要很多，但要允许 HTTP 查询并发。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate 给老库补 delay_s 列（Python 版早期版本没有这一列）。
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

	for _, want := range []string{"delay_s"} {
		found := false
		for _, c := range cols {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			if _, err := db.Exec("ALTER TABLE messages ADD COLUMN delay_s INTEGER"); err != nil {
				return fmt.Errorf("添加 %s 列失败: %w", want, err)
			}
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
//  2. 自己发出的短信，5 分钟内同号码同正文 —— 发送接口已写过库，视为已归档
//  3. 否则插入新行
func (s *Store) Archive(m Message) (bool, error) {
	m.Hash = ContentHash(m.PDUType, m.Peer, m.TS, m.Body)

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
		var recent int
		gerr := s.db.QueryRow(
			"SELECT 1 FROM messages WHERE dir='out' AND peer=? AND body=? "+
				"AND created_at >= datetime('now','localtime','-5 minutes') LIMIT 1",
			m.Peer, m.Body).Scan(&recent)
		if gerr == nil {
			return true, nil
		}
		if gerr != sql.ErrNoRows {
			return false, gerr
		}
	}

	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO messages
		   (dir, peer, body, ts, state, storage, smsc, pdu_type, mm_path, hash, delay_s)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.Dir, m.Peer, m.Body, m.TS, m.State, m.Storage, m.SMSC, m.PDUType, m.MMPath, m.Hash, m.DelayS)
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

const selectAll = `SELECT id, dir, peer, body, ts, state, storage, smsc, pdu_type,
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
	err := r.Scan(&m.ID, &m.Dir, &peer, &body, &ts, &state, &storage, &smsc,
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

// ListQuery 是 /api/messages 的查询条件。
type ListQuery struct {
	Dir    string
	Q      string
	Limit  int
	Offset int
}

// List 返回分页结果与总数。
func (s *Store) List(q ListQuery) ([]Message, int, error) {
	where := "WHERE 1=1"
	var args []interface{}
	if q.Dir == "in" || q.Dir == "out" {
		where += " AND dir=?"
		args = append(args, q.Dir)
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
}

// Counts 返回累计统计。
func (s *Store) Counts() (Counts, error) {
	var c Counts
	var inbox, sent, pending sql.NullInt64
	var total int
	// 空表时 SUM() 返回 NULL，COUNT(*) 返回 0，所以前三个用 NullInt64 接。
	err := s.db.QueryRow(
		"SELECT SUM(dir='in'), SUM(dir='out'), COUNT(*), SUM(dir='in' AND forwarded=0) FROM messages").
		Scan(&inbox, &sent, &total, &pending)
	if err != nil {
		return c, err
	}
	c.Inbox, c.Sent, c.Total = int(inbox.Int64), int(sent.Int64), total
	c.Pending = int(pending.Int64)
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
// 运行时设置
// ---------------------------------------------------------------------------

// GetSetting 读取一个设置项。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting 写入或覆盖一个设置项。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   value=excluded.value,
		   updated_at=datetime('now','localtime')`,
		key, value)
	return err
}

// DeleteSetting 删除一个设置项（回到默认行为）。
func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec("DELETE FROM settings WHERE key=?", key)
	return err
}

// AllSettings 返回全部设置项。
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 转发通道
// ---------------------------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const channelCols = "id, name, type, enabled, config"

func scanChannel(r rowScanner) (*forward.Channel, error) {
	var (
		c       forward.Channel
		enabled int
		cfgJSON string
	)
	if err := r.Scan(&c.ID, &c.Name, &c.Type, &enabled, &cfgJSON); err != nil {
		return nil, err
	}
	c.Enabled = enabled != 0
	if cfgJSON != "" {
		_ = json.Unmarshal([]byte(cfgJSON), &c.Config)
	}
	return &c, nil
}

// ListChannels 列出通道。enabledOnly 为 true 时只返回启用的。
func (s *Store) ListChannels(enabledOnly bool) ([]forward.Channel, error) {
	q := "SELECT " + channelCols + " FROM channels"
	if enabledOnly {
		q += " WHERE enabled=1"
	}
	q += " ORDER BY id"

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []forward.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetChannel 按 id 取一条通道。
func (s *Store) GetChannel(id int64) (*forward.Channel, error) {
	row := s.db.QueryRow("SELECT "+channelCols+" FROM channels WHERE id=?", id)
	c, err := scanChannel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// CreateChannel 新建通道，成功后回填 ID。
func (s *Store) CreateChannel(c *forward.Channel) error {
	b, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		"INSERT INTO channels (name, type, enabled, config) VALUES (?,?,?,?)",
		c.Name, c.Type, boolToInt(c.Enabled), string(b))
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

// UpdateChannel 更新通道。
func (s *Store) UpdateChannel(c *forward.Channel) error {
	b, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		"UPDATE channels SET name=?, type=?, enabled=?, config=? WHERE id=?",
		c.Name, c.Type, boolToInt(c.Enabled), string(b), c.ID)
	return err
}

// DeleteChannel 删除通道及其投递记录。
func (s *Store) DeleteChannel(id int64) error {
	if _, err := s.db.Exec("DELETE FROM channels WHERE id=?", id); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM channel_deliveries WHERE channel_id=?", id)
	return err
}

// ChannelCount 返回通道总数（用于判断是否需要从 config.json 迁移）。
func (s *Store) ChannelCount() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM channels").Scan(&n)
	return n, err
}

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
		 VALUES (?,?,?,1,?,datetime('now','localtime'))
		 ON CONFLICT(message_id, channel_id) DO UPDATE SET
		   status=excluded.status,
		   attempts=attempts+1,
		   last_error=excluded.last_error,
		   updated_at=excluded.updated_at`,
		messageID, channelID, status, errMsg)
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

// ---------------------------------------------------------------------------
// DDNS 记录
// ---------------------------------------------------------------------------

// 用 IFNULL 把可空列统一成空字符串，避免 Scan 到 NULL 报错。
const ddnsCols = `id, name, type, IFNULL(zone_id,''), api_token, source,
       proxied, ttl, enabled, IFNULL(last_ip,''), IFNULL(last_update,''), IFNULL(last_error,'')`

func scanDDNS(r rowScanner) (*ddns.Record, error) {
	var (
		rec     ddns.Record
		proxied int
		enabled int
	)
	err := r.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ZoneID, &rec.APIToken,
		&rec.Source, &proxied, &rec.TTL, &enabled,
		&rec.LastIP, &rec.LastUpdate, &rec.LastError)
	if err != nil {
		return nil, err
	}
	rec.Proxied = proxied != 0
	rec.Enabled = enabled != 0
	return &rec, nil
}

// ListDDNS 列出全部 DDNS 记录。
func (s *Store) ListDDNS() ([]ddns.Record, error) {
	rows, err := s.db.Query("SELECT " + ddnsCols + " FROM ddns ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ddns.Record
	for rows.Next() {
		r, err := scanDDNS(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetDDNS 按 id 取记录。
func (s *Store) GetDDNS(id int64) (*ddns.Record, error) {
	row := s.db.QueryRow("SELECT "+ddnsCols+" FROM ddns WHERE id=?", id)
	r, err := scanDDNS(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// CreateDDNS 新建记录。
func (s *Store) CreateDDNS(r *ddns.Record) error {
	res, err := s.db.Exec(
		`INSERT INTO ddns (name, type, zone_id, api_token, source, proxied, ttl, enabled)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.Name, r.Type, r.ZoneID, r.APIToken, r.Source,
		boolToInt(r.Proxied), r.TTL, boolToInt(r.Enabled))
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// UpdateDDNS 更新记录配置。
func (s *Store) UpdateDDNS(r *ddns.Record) error {
	_, err := s.db.Exec(
		`UPDATE ddns SET name=?, type=?, zone_id=?, api_token=?, source=?,
		   proxied=?, ttl=?, enabled=? WHERE id=?`,
		r.Name, r.Type, r.ZoneID, r.APIToken, r.Source,
		boolToInt(r.Proxied), r.TTL, boolToInt(r.Enabled), r.ID)
	return err
}

// DeleteDDNS 删除记录。
func (s *Store) DeleteDDNS(id int64) error {
	_, err := s.db.Exec("DELETE FROM ddns WHERE id=?", id)
	return err
}

// UpdateDDNSResult 记录一次同步结果。
func (s *Store) UpdateDDNSResult(id int64, ip, errMsg string) error {
	if errMsg != "" {
		_, err := s.db.Exec(
			"UPDATE ddns SET last_error=?, last_update=datetime('now','localtime') WHERE id=?",
			errMsg, id)
		return err
	}
	_, err := s.db.Exec(
		"UPDATE ddns SET last_ip=?, last_error=NULL, last_update=datetime('now','localtime') WHERE id=?",
		ip, id)
	return err
}
