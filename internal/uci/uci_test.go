package uci

import (
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	const src = `
# 注释行
config smsrelay 'main'
	option enabled '1'
	option listen_port '8088'
	option api_token ''

config channel 'ch1'
	option name '运维'
	list body_line '{"text":'
	list body_line ' "%s"}'
`
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(f.Sections) != 2 {
		t.Fatalf("段数 = %d, want 2", len(f.Sections))
	}
	main := f.First("smsrelay")
	if main.Name != "main" || main.Get("listen_port") != "8088" {
		t.Fatalf("main 段解析错误: %+v", main)
	}
	if _, ok := main.Options["api_token"]; !ok {
		t.Fatal("空值 option 也应存在")
	}
	ch := f.SectionsOf("channel")[0]
	lines := ch.List("body_line")
	if len(lines) != 2 || lines[1] != ` "%s"}` {
		t.Fatalf("list 解析错误: %#v", lines)
	}
}

// 引号内的空格、井号、逗号都不能被当作分隔或注释。
func TestParseQuotedValues(t *testing.T) {
	const src = `
config smsrelay 'main'
	option prefix '【短信】 #1, 通知'
	option url "https://example.com/a b?x=1&y=2"
`
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	s := f.First("smsrelay")
	if got := s.Get("prefix"); got != "【短信】 #1, 通知" {
		t.Fatalf("prefix = %q", got)
	}
	if got := s.Get("url"); got != "https://example.com/a b?x=1&y=2" {
		t.Fatalf("url = %q", got)
	}
}

// 单引号里的 \' 与双引号里的 \" 要还原成引号本身；其它反斜杠原样保留。
func TestParseEscapes(t *testing.T) {
	const src = `
config x 'y'
	option a 'it\'s ok'
	option b "say \"hi\""
	option c 'C:\path'
`
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	s := f.First("x")
	if s.Get("a") != "it's ok" {
		t.Fatalf("a = %q", s.Get("a"))
	}
	if s.Get("b") != `say "hi"` {
		t.Fatalf("b = %q", s.Get("b"))
	}
	if s.Get("c") != `C:\path` {
		t.Fatalf("c = %q（UCI 不做 \\n 之类的展开）", s.Get("c"))
	}
}

func TestParseAnonymousSection(t *testing.T) {
	f, err := Parse(strings.NewReader("config channel\n\toption name 'x'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Sections[0].Name != "" {
		t.Fatalf("匿名段名应为空, got %q", f.Sections[0].Name)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		"option a 'b'\n", // 出现在 config 之前
		"config\n",       // 缺类型
		"config x 'y'\noption a 'unterminated\n",
		"unknown a b\n",
	} {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Fatalf("应报错: %q", src)
		}
	}
}
