// Package forward 负责把短信推送到外部通道（钉钉机器人 / 通用 webhook）。
//
// 与早期单通道实现的区别：
//   - 通道配置存在 SQLite，界面上可增删改，改完立即生效，不用重启
//   - 一条短信会投递给所有启用的通道，每个通道各自记录成功/失败
//   - 正文用模板渲染，可带本机号码（这也是「转发要显示收信号码」的实现方式）
package forward

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 通道类型。
const (
	TypeDingTalk = "dingtalk"
	TypeWebhook  = "webhook"
)

// Config 是通道的具体配置。不同类型用到的字段不同，统一放一个结构里，
// 便于整体 JSON 存取。
type Config struct {
	URL string `json:"url"`
	// 钉钉「加签」模式的密钥（SEC 开头）。为空表示用关键词或 IP 白名单模式。
	Secret string `json:"secret,omitempty"`
	// 正文前缀，例如「【短信】」
	Prefix string `json:"prefix,omitempty"`

	// webhook 专用
	Method       string            `json:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	BodyTemplate string            `json:"body_template,omitempty"`
}

// Channel 是一条转发通道。
type Channel struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	Config  Config `json:"config"`

	// 最近一次投递结果，仅用于界面展示，不从数据库读
	LastError string `json:"last_error,omitempty"`
}

// Vars 是模板变量。键名与 {{xxx}} 占位符对应。
type Vars struct {
	OwnNumber string // 本机号码（收信号码）
	Number    string // 发送方号码
	Body      string // 正文
	TS        string // 时间
	ID        int64  // 记录 id
	Direction string // in / out
	Prefix    string // 通道配置的前缀，由 Send 填入，不用于其它场景
}

// 默认模板：带本机号码。这正是「转发内容里要有收信号码」的落地方式。
const defaultTemplate = "{{prefix}}收到短信\n收信号码：{{own_number}}\n发送号码：{{number}}\n内容：{{body}}\n时间：{{ts}}"

// DefaultWebhookTemplate 是 webhook 通道没填正文模板时的兜底。
//
// 必须是**合法 JSON**：sendWebhook 以 application/json 发送，如果回退到
// defaultTemplate（纯文本），收端会拿到一段无法解析的「JSON」——
// 早期就是这么错的，而且报错发生在对方系统里，很难查。
const DefaultWebhookTemplate = `{"own_number":{{own_number_json}},"from":{{number_json}},` +
	`"text":{{body_json}},"ts":{{ts_json}},"id":{{id}},"dir":{{dir_json}}}`

var placeholderRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// Render 渲染模板。未知占位符原样保留，便于排查拼写错误。
//
// 每个字段都提供 xxx 与 xxx_json 两种形式：前者直接内插（用于纯文本模板），
// 后者是带引号的 JSON 字符串字面量（用于 JSON 模板）。
// 阶段一次性替换，替换进去的内容不会被再次当成占位符解析 ——
// 这样短信正文里出现 "{{number}}" 也不会被二次替换。
func Render(tmpl string, v Vars) string {
	if tmpl == "" {
		tmpl = defaultTemplate
	}
	own := v.OwnNumber
	if own == "" {
		own = "未知"
	}
	// 前缀带尾随空格，模板里写 {{prefix}}收到短信 才能得到 "【短信】 收到短信"
	prefix := ""
	if p := strings.TrimSpace(v.Prefix); p != "" {
		prefix = p + " "
	}

	vars := map[string]string{
		"own_number": own,
		"number":     v.Number,
		"body":       v.Body,
		"ts":         v.TS,
		"id":         strconv.FormatInt(v.ID, 10),
		"dir":        v.Direction,
		"prefix":     prefix,

		"own_number_json": jsonString(own),
		"number_json":     jsonString(v.Number),
		"body_json":       jsonString(v.Body),
		"ts_json":         jsonString(v.TS),
		"dir_json":        jsonString(v.Direction),
		"prefix_json":     jsonString(strings.TrimSpace(prefix)),
	}
	return placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := m[2 : len(m)-2]
		if val, ok := vars[key]; ok {
			return val
		}
		return m
	})
}

// jsonString 把正文转成**带引号**的 JSON 字符串字面量。
//
// 这样模板写 {"text": {{body_json}}} 就能直接拼出合法 JSON。
// 早期实现去掉了首尾引号，导致模板必须写成 "{{body_json}}" 才对，
// 而按直觉写（不加引号）会产出 `"text":正文` 这种非法 JSON —— 踩过。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// Sender 是 http 客户端，带连接复用。
type Sender struct {
	client *http.Client
}

// NewSender 创建发送器。
func NewSender() *Sender {
	return &Sender{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Send 按通道类型投递一条短信，返回错误表示失败。
func (s *Sender) Send(ch Channel, v Vars) error {
	// 前缀来自通道配置，统一交给模板渲染，避免"先渲染再字符串替换"这种两段式处理
	v.Prefix = ch.Config.Prefix
	switch ch.Type {
	case TypeDingTalk:
		return s.sendDingTalk(ch, v)
	case TypeWebhook:
		return s.sendWebhook(ch, v)
	default:
		return fmt.Errorf("不支持的通道类型: %s", ch.Type)
	}
}

func (s *Sender) sendDingTalk(ch Channel, v Vars) error {
	if strings.TrimSpace(ch.Config.URL) == "" {
		return fmt.Errorf("webhook 地址为空")
	}

	content := Render(defaultTemplate, v)

	payload := map[string]interface{}{
		"msgtype": "text",
		"text":    map[string]string{"content": truncateRunes(content, 1900)},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	target := ch.Config.URL
	// 加签模式：在 URL 上追加 timestamp 与 sign
	if sec := strings.TrimSpace(ch.Config.Secret); sec != "" {
		ts, sign := dingTalkSign(sec)
		sep := "&"
		if !strings.Contains(target, "?") {
			sep = "?"
		}
		target = target + sep + "timestamp=" + ts + "&sign=" + sign
	}

	return s.post(target, "application/json; charset=utf-8", nil, body, ch.Name)
}

func (s *Sender) sendWebhook(ch Channel, v Vars) error {
	if strings.TrimSpace(ch.Config.URL) == "" {
		return fmt.Errorf("webhook 地址为空")
	}

	// 没配模板时用合法 JSON 兜底，绝不回退到纯文本模板
	tmpl := ch.Config.BodyTemplate
	if strings.TrimSpace(tmpl) == "" {
		tmpl = DefaultWebhookTemplate
	}
	body := Render(tmpl, v)

	method := strings.ToUpper(strings.TrimSpace(ch.Config.Method))
	if method == "" {
		method = http.MethodPost
	}

	contentType := "application/json; charset=utf-8"
	headers := map[string]string{}
	for k, val := range ch.Config.Headers {
		if strings.EqualFold(k, "Content-Type") {
			contentType = val
			continue
		}
		headers[k] = val
	}

	req, err := http.NewRequest(method, ch.Config.URL, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, val := range headers {
		req.Header.Set(k, val)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	return nil
}

func (s *Sender) post(target, contentType string, headers map[string]string, body []byte, name string) error {
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, val := range headers {
		req.Header.Set(k, val)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}

	// 钉钉的错误是 HTTP 200 + errcode != 0，必须解析才有意义
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &r); err == nil && r.ErrCode != 0 {
		return fmt.Errorf("钉钉错误 %d: %s", r.ErrCode, r.ErrMsg)
	}
	return nil
}

// dingTalkSign 按钉钉文档计算加签：HmacSHA256(timestamp+"\n"+secret, secret) 再 base64+urlencode。
func dingTalkSign(secret string) (string, string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return ts, url.QueryEscape(sign)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// MaskSecret 把 URL 里的 access_token 与自定义 header 的值脱敏，
// 用于 GET /api/channels 时返回给前端（后端写库时仍保留原值）。
func MaskSecret(cfg Config) Config {
	out := cfg
	out.URL = maskURL(out.URL)
	if out.Secret != "" {
		out.Secret = maskTail(out.Secret)
	}
	if len(out.Headers) > 0 {
		h := make(map[string]string, len(out.Headers))
		for k, v := range out.Headers {
			h[k] = maskTail(v)
		}
		out.Headers = h
	}
	return out
}

var tokenRe = regexp.MustCompile(`(access_token=)([^&]+)`)

func maskURL(s string) string {
	return tokenRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := tokenRe.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		return parts[1] + maskTail(parts[2])
	})
}

// maskTail 保留末 4 位，其余用 * 代替。太短的值整段遮掉。
func maskTail(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return "****" + string(r[len(r)-4:])
}

// IsMasked 判断前端回传的值是不是脱敏占位，是的话应保留数据库里的原值。
func IsMasked(s string) bool {
	return strings.HasPrefix(s, "****")
}
