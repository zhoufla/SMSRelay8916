package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "smsrelay")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `
config smsrelay 'main'
	option enabled '1'
	option listen_port '9088'
	option api_token 'secret-token'
	option retain_days '7'
	option own_number '13800138000'

config channel 'ch_ops'
	option name '运维群'
	option type 'dingtalk'
	option enabled '1'
	option url 'https://oapi.dingtalk.com/robot/send?access_token=x'
	option secret 'SECxxx'
	option prefix '【短信】'
	option match_code_only '1'
	list header 'Content-Type: application/json'

config channel 'ch_hook'
	option name '自建 webhook'
	option enabled '0'
	option url 'https://example.com/hook'
	list body_line '{"text":"%s"}'

config ddns 'd_home'
	option name 'home.example.com'
	option type 'A'
	option api_token 'cf-token'
	option source 'iface:wwan0'
	option enabled '1'
`

func TestLoad(t *testing.T) {
	c, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !c.Enabled {
		t.Fatalf("main 解析错误: %+v", c)
	}
	if c.RetainDays != 7 || c.OwnNumber != "13800138000" {
		t.Fatalf("retain/own_number 解析错误: %+v", c)
	}
	if len(c.Channels) != 2 {
		t.Fatalf("通道数 = %d, want 2", len(c.Channels))
	}
	if c.Channels[0].Name != "运维群" || c.Channels[0].Type != "dingtalk" {
		t.Fatalf("通道字段错误: %+v", c.Channels[0])
	}
	if c.Channels[0].Config.Headers["Content-Type"] != "application/json" {
		t.Fatalf("header 解析错误: %#v", c.Channels[0].Config.Headers)
	}
	if !c.Channels[0].Config.MatchCodeOnly {
		t.Fatal("match_code_only 应为 true")
	}
	if c.Channels[1].Config.BodyTemplate != `{"text":"%s"}` {
		t.Fatalf("body_line 拼接错误: %q", c.Channels[1].Config.BodyTemplate)
	}
	if len(c.EnabledChannels()) != 1 {
		t.Fatalf("启用通道数 = %d, want 1", len(c.EnabledChannels()))
	}
	if len(c.DDNS) != 1 || c.DDNS[0].Name != "home.example.com" || c.DDNS[0].Source != "iface:wwan0" {
		t.Fatalf("DDNS 解析错误: %+v", c.DDNS)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("不应有告警: %v", c.Warnings)
	}
}

// 段名是投递历史的关联键：同名段无论出现在第几个，主键必须一致。
func TestSectionIDStable(t *testing.T) {
	if SectionID("ch_ops") != SectionID("ch_ops") {
		t.Fatal("同一段名必须得到同一主键")
	}
	if SectionID("ch_ops") == SectionID("ch_hook") {
		t.Fatal("不同段名不应撞主键")
	}
	if SectionID("x") < 0 {
		t.Fatal("主键必须为正，避免 API 里与 0/负数混淆")
	}
}

// 配置文件不存在时应给出可用的默认值，而不是启动失败。
func TestLoadMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("缺失文件不应报错: %v", err)
	}
	if c.DBPath != "/etc/smsrelay/sms.db" {
		t.Fatalf("默认值不对: %+v", c)
	}
}

// 启用但没填 url 的通道要给出告警，方便在日志里定位"配了却不发"的原因。
func TestChannelWithoutURLWarns(t *testing.T) {
	p := writeTemp(t, "config channel 'c1'\n\toption enabled '1'\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) == 0 {
		t.Fatal("缺少 url 应产生告警")
	}
}

// 旧固件里的段类型是 `config main 'main'`（那时只渲染 config.json）。
// 升级后必须还能读出原数据库路径、本机号码和扫描间隔。
func TestLoadLegacyMainSection(t *testing.T) {
	const legacy = `
config main 'main'
	option enabled '1'
	option listen_host '0.0.0.0'
	option listen_port '8088'
	option api_token 'legacy-token'
	option db_path '/etc/smsrelay/sms.db'
	option guard_interval '90'
	option modem_index '0'
	option own_number '13900139000'
`
	c, err := Load(writeTemp(t, legacy))
	if err != nil {
		t.Fatalf("加载旧版配置失败: %v", err)
	}
	if c.OwnNumber != "13900139000" || c.GuardInterval != 90 {
		t.Fatalf("旧段类型其余字段解析错误: %+v", c)
	}
}
