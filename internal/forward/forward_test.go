package forward

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func baseVars() Vars {
	return Vars{
		OwnNumber: "13800000000",
		Number:    "10086",
		Body:      "含\"引号\"、逗号,与\n换行",
		TS:        "2026-01-01T00:00:00+0800",
		ID:        7,
		Direction: "in",
	}
}

// body_json 必须自带引号：模板写 {"text": {{body_json}}} 要能直接解析。
// 早期实现去掉了引号，产出 "text":正文 这种非法 JSON。
func TestRenderBodyJSONIsQuoted(t *testing.T) {
	v := baseVars()
	out := Render(`{"text": {{body_json}}}`, v)

	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("渲染结果不是合法 JSON: %v\n输出: %s", err, out)
	}
	if m["text"] != v.Body {
		t.Fatalf("正文被破坏: got %q want %q", m["text"], v.Body)
	}
}

// 所有 *_json 占位符都应该是合法 JSON 片段。
func TestRenderJSONVariants(t *testing.T) {
	out := Render(
		`{"own":{{own_number_json}},"from":{{number_json}},"ts":{{ts_json}},"dir":{{dir_json}}}`,
		baseVars())

	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("JSON 片段不合法: %v\n输出: %s", err, out)
	}
	if m["own"] != "13800000000" || m["from"] != "10086" {
		t.Fatalf("字段值不对: %+v", m)
	}
}

// 替换进去的内容不能被二次当成占位符解析。
func TestRenderNoDoubleSubstitution(t *testing.T) {
	v := Vars{Number: "10086", Body: "{{number}}"}
	if got := Render("{{body}}", v); got != "{{number}}" {
		t.Fatalf("发生了二次替换: got %q", got)
	}
}

// 未知占位符原样保留，便于排查拼写错误。
func TestRenderKeepsUnknown(t *testing.T) {
	if got := Render("a{{nope}}b", baseVars()); got != "a{{nope}}b" {
		t.Fatalf("未知占位符被吞掉: %q", got)
	}
}

// 前缀来自通道配置，由 Send 注入后再渲染（不再做二次字符串替换）。
func TestRenderPrefix(t *testing.T) {
	if got := Render("{{prefix}}hi", Vars{Prefix: "【短信】"}); got != "【短信】 hi" {
		t.Fatalf("前缀渲染不对: %q", got)
	}
	if got := Render("{{prefix}}hi", Vars{}); got != "hi" {
		t.Fatalf("空前缀应渲染为空: %q", got)
	}
}

// 回归：webhook 没填 body_template 时必须回退到合法 JSON，
// 而不是 defaultTemplate 那段纯文本（否则对方系统收到无法解析的「JSON」）。
func TestWebhookWithoutTemplateSendsValidJSON(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewSender()
	err := s.Send(Channel{
		Type:   TypeWebhook,
		Config: Config{URL: srv.URL}, // 故意不设 BodyTemplate
	}, baseVars())
	if err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("默认模板产出的不是合法 JSON: %v\n body: %s", err, got)
	}
	if m["text"] != baseVars().Body {
		t.Fatalf("正文不对: %v", m["text"])
	}
	if m["own_number"] != "13800000000" {
		t.Fatalf("默认模板必须带本机号码: %v", m["own_number"])
	}
}

// 默认 webhook 模板本身要能通过 JSON 校验。
func TestDefaultWebhookTemplateParses(t *testing.T) {
	if !json.Valid([]byte(Render(DefaultWebhookTemplate, baseVars()))) {
		t.Fatalf("DefaultWebhookTemplate 渲染后不是合法 JSON")
	}
	if !strings.Contains(DefaultWebhookTemplate, "{{body_json}}") {
		t.Fatalf("默认模板应使用 body_json")
	}
}

// 带验证码时空模板选带码默认版：首行「【验证码】发件人」，正文，末行手机号。
func TestRenderDefaultWithCode(t *testing.T) {
	v := baseVars()
	v.Code = "483921"
	out := Render("", v)
	want := "【483921】10086\n" + v.Body + "\n手机号：13800000000"
	if out != want {
		t.Fatalf("带码默认模板不对:\n got %q\nwant %q", out, want)
	}
}

// 不带验证码时三行：发件人、正文、手机号，且不出现验证码字样。
func TestRenderDefaultWithoutCode(t *testing.T) {
	v := baseVars()
	out := Render("", v)
	want := "10086\n" + v.Body + "\n手机号：13800000000"
	if out != want {
		t.Fatalf("默认模板不对:\n got %q\nwant %q", out, want)
	}
	if strings.Contains(out, "验证码") || strings.Contains(out, "收到短信") {
		t.Fatalf("无码消息不应出现验证码/收到短信行: %q", out)
	}
}

// code_json 与其它 *_json 一样要自带引号。
func TestRenderCodeJSON(t *testing.T) {
	v := baseVars()
	v.Code = "483921"
	out := Render(`{"code":{{code_json}}}`, v)
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("code_json 产出非法 JSON: %v\n输出: %s", err, out)
	}
	if m["code"] != "483921" {
		t.Fatalf("code 不对: %q", m["code"])
	}
	// 空验证码渲染成空字符串，仍然是合法 JSON
	out2 := Render(`{"code":{{code_json}}}`, baseVars())
	var m2 map[string]string
	if err := json.Unmarshal([]byte(out2), &m2); err != nil {
		t.Fatalf("空 code_json 产出非法 JSON: %v", err)
	}
	if m2["code"] != "" {
		t.Fatalf("空 code 应渲染为空串: %q", m2["code"])
	}
}

// 条件匹配：空条件 = 全过（向后兼容老配置）；非空条件 AND；
// 排除关键词优先于命中关键词；code_only 对出站与无码消息恒 false。
func TestMatchRule(t *testing.T) {
	codeIn := baseVars() // 10086, in
	codeIn.Code = "483921"
	codeIn.Body = "您的验证码483921，2分钟内有效"

	cases := []struct {
		name string
		cfg  Config
		v    Vars
		want bool
	}{
		{"无条件全过", Config{}, codeIn, true},
		{"无条件全过-出站", Config{}, baseOut(), true},
		{"前缀命中", Config{MatchPeer: "106,10086"}, codeIn, true},
		{"前缀未命中", Config{MatchPeer: "106"}, codeIn, false},
		{"前缀是前缀不是全等", Config{MatchPeer: "1008"}, codeIn, true},
		{"关键词命中", Config{MatchKeyword: "验证码,动态码"}, Vars{Body: "您的验证码483921", Direction: "in"}, true},
		{"关键词未命中", Config{MatchKeyword: "验证码"}, Vars{Body: "明天开会", Direction: "in"}, false},
		{"排除优先于命中", Config{MatchKeyword: "短信", MatchSkipKeyword: "退订"}, Vars{Body: "短信通知，退订回T", Direction: "in"}, false},
		{"仅验证码-命中", Config{MatchCodeOnly: true}, codeIn, true},
		{"仅验证码-出站恒false", Config{MatchCodeOnly: true}, codeOutWithFakeCode(), false},
		{"仅验证码-无码false", Config{MatchCodeOnly: true}, baseVars(), false},
		{"AND组合-全中", Config{MatchPeer: "10086", MatchKeyword: "验证码", MatchCodeOnly: true}, codeIn, true},
		{"AND组合-缺一不可", Config{MatchPeer: "10086", MatchKeyword: "验证码", MatchCodeOnly: true}, baseVars(), false},
		{"条件项空白容忍", Config{MatchPeer: " 106 , 10086 "}, Vars{Number: "10658", Direction: "in"}, true},
	}
	for _, tc := range cases {
		if got := tc.cfg.MatchRule(tc.v); got != tc.want {
			t.Errorf("%s: MatchRule = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// baseOut 返回一个出站方向的基准消息。
func baseOut() Vars {
	v := baseVars()
	v.Direction = "out"
	return v
}

// codeOutWithFakeCode 构造"出站但带码"的异常样例：code_only 必须同时要求入站。
func codeOutWithFakeCode() Vars {
	v := baseOut()
	v.Code = "1234"
	return v
}

// Channel.Matches 是 MatchRule 的薄封装，保证两者不会脱节。
func TestChannelMatches(t *testing.T) {
	ch := Channel{Config: Config{MatchCodeOnly: true}}
	v := baseVars()
	v.Code = "1234"
	if !ch.Matches(v) {
		t.Fatal("带码入站应命中")
	}
	if ch.Matches(baseVars()) {
		t.Fatal("无码不应命中")
	}
}
