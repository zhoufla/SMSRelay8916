// Package config 把 UCI 文件映射成 smsrelay 的运行配置。
//
// 配置的唯一来源是 /etc/config/smsrelay。程序只读不写：改配置走 LuCI
// （rpcd → uci），改完 uci commit 会通过 procd 的 reload trigger 重启本服务。
//
// 通道与 DDNS 记录用段名做稳定主键：段名由 LuCI 生成后不再变化，
// 因此库里按 channel_id 记录的投递历史不会因为改名/改顺序而错位。
package config

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/zhoufla/SMSRelay8916/internal/ddns"
	"github.com/zhoufla/SMSRelay8916/internal/forward"
	"github.com/zhoufla/SMSRelay8916/internal/uci"
)

// DefaultPath 是 UCI 配置的默认位置。
const DefaultPath = "/etc/config/smsrelay"

// Config 是程序运行所需的全部配置。
type Config struct {
	Source string // 配置来源文件，仅用于日志

	Enabled       bool
	Keepalive     bool
	DBPath        string
	RetainDays    int
	OwnNumber     string
	GuardInterval int
	ModemIndex    string

	Channels []forward.Channel
	DDNS     []ddns.Record

	// Warnings 记录可以继续运行、但值得提示的问题（如通道缺 URL）。
	Warnings []string
}

// Load 读取并解析配置文件。文件缺失时返回一套可用的默认值。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	f, err := uci.Load(path)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Source:        path,
		Enabled:       true,
		Keepalive:     true,
		DBPath:        "/etc/smsrelay/sms.db",
		RetainDays:    30,
		GuardInterval: 60,
		ModemIndex:    "0",
	}

	main := f.First("smsrelay")
	if main == nil {
		// 兼容旧版配置：早期 init 渲染 config.json 时用的是 `config main 'main'`。
		// 不认这个段类型会让 own_number / db_path 退回默认值，必须兼容读取。
		main = f.First("main")
	}
	if main != nil {
		c.Enabled = boolOpt(main, "enabled", true)
		c.Keepalive = boolOpt(main, "keepalive", true)
		c.DBPath = strOpt(main, "db_path", c.DBPath)
		c.RetainDays = intOpt(main, "retain_days", c.RetainDays)
		c.OwnNumber = main.Get("own_number")
		c.GuardInterval = intOpt(main, "guard_interval", c.GuardInterval)
		c.ModemIndex = strOpt(main, "modem_index", c.ModemIndex)
	}

	for i, s := range f.SectionsOf("channel") {
		ch, warn := parseChannel(s, i)
		if warn != "" {
			c.Warnings = append(c.Warnings, warn)
		}
		c.Channels = append(c.Channels, ch)
	}
	for _, s := range f.SectionsOf("ddns") {
		c.DDNS = append(c.DDNS, parseDDNS(s))
	}

	if c.GuardInterval <= 0 {
		c.GuardInterval = 60
	}
	if c.RetainDays < 0 {
		c.RetainDays = 0
	}
	return c, nil
}

// EnabledChannels 返回启用的通道。
func (c *Config) EnabledChannels() []forward.Channel {
	if c == nil {
		return nil
	}
	out := make([]forward.Channel, 0, len(c.Channels))
	for _, ch := range c.Channels {
		if ch.Enabled {
			out = append(out, ch)
		}
	}
	return out
}

// ChannelByID 按主键找通道。
func (c *Config) ChannelByID(id int64) *forward.Channel {
	if c == nil {
		return nil
	}
	for i := range c.Channels {
		if c.Channels[i].ID == id {
			return &c.Channels[i]
		}
	}
	return nil
}

// DDNSByID 按主键找 DDNS 记录。
func (c *Config) DDNSByID(id int64) *ddns.Record {
	if c == nil {
		return nil
	}
	for i := range c.DDNS {
		if c.DDNS[i].ID == id {
			return &c.DDNS[i]
		}
	}
	return nil
}

// SectionID 把 UCI 段名映射成稳定的 int64 主键。
//
// 用 FNV-1a 而不是序号：段顺序会随 LuCI 编辑变化，序号一变，
// channel_deliveries 里"哪条短信发到了哪个通道"就全错位了。
func SectionID(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64() &^ (1 << 63))
}

func parseChannel(s *uci.Section, idx int) (forward.Channel, string) {
	name := s.Name
	if name == "" {
		// 手写配置时可能省掉段名，用序号兜底（仍保持稳定，因为顺序不变）
		name = "channel_" + strconv.Itoa(idx)
	}
	ch := forward.Channel{
		ID:      SectionID(name),
		Name:    strOpt(s, "name", name),
		Type:    strOpt(s, "type", forward.TypeWebhook),
		Enabled: boolOpt(s, "enabled", true),
		Config: forward.Config{
			URL:              s.Get("url"),
			Secret:           s.Get("secret"),
			Prefix:           s.Get("prefix"),
			Method:           strOpt(s, "method", "POST"),
			BodyTemplate:     bodyTemplate(s),
			Headers:          headers(s),
			MatchPeer:        s.Get("match_peer"),
			MatchKeyword:     s.Get("match_keyword"),
			MatchSkipKeyword: s.Get("match_skip_keyword"),
			MatchCodeOnly:    boolOpt(s, "match_code_only", false),
		},
	}
	if ch.Enabled && ch.Config.URL == "" {
		return ch, fmt.Sprintf("通道 %q 启用但缺少 url，将被跳过", ch.Name)
	}
	return ch, ""
}

// bodyTemplate 优先取 list body_line（多行模板不必转义），
// 回退到 option body_template（兼容手写配置）。
func bodyTemplate(s *uci.Section) string {
	if lines := s.List("body_line"); len(lines) > 0 {
		return strings.Join(lines, "\n")
	}
	return s.Get("body_template")
}

// headers 解析 list header 'K: V'。
func headers(s *uci.Section) map[string]string {
	items := s.List("header")
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		k, v, ok := strings.Cut(it, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseDDNS(s *uci.Section) ddns.Record {
	name := s.Name
	if name == "" {
		name = "ddns_" + s.Get("name")
	}
	return ddns.Record{
		ID:       SectionID(name),
		Name:     s.Get("name"),
		Type:     strOpt(s, "type", "A"),
		ZoneID:   s.Get("zone_id"),
		APIToken: s.Get("api_token"),
		Source:   strOpt(s, "source", "external"),
		Proxied:  boolOpt(s, "proxied", false),
		TTL:      intOpt(s, "ttl", 1),
		Enabled:  boolOpt(s, "enabled", true),
	}
}

func strOpt(s *uci.Section, key, def string) string {
	if v := s.Get(key); v != "" {
		return v
	}
	return def
}

func intOpt(s *uci.Section, key string, def int) int {
	v := s.Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// boolOpt 接受 UCI 常见的几种写法：1/0、true/false、yes/no、on/off。
func boolOpt(s *uci.Section, key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s.Get(key))) {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return def
	}
}
