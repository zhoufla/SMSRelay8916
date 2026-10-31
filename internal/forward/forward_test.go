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

// 带验证码时空模板选带码默认版，验证码独立成行、在最前。
func TestRenderDefaultWithCode(t *testing.T) {
	v := baseVars()
	v.Code = "483921"
	out := Render("", v)
	if !strings.HasPrefix(out, "收到短信\n验证码：483921\n") {
		t.Fatalf("带码默认模板首行应是验证码: %q", out)
	}
}

// 不带验证码时绝不能出现空的「验证码：」行。
func TestRenderDefaultWithoutCode(t *testing.T) {
	out := Render("", baseVars())
	if strings.Contains(out, "验证码") {
		t.Fatalf("无码消息不应出现验证码行: %q", out)
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
