// Package relay 负责把短信推送到外部通道。当前实现钉钉机器人。
package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DingTalk 是钉钉群机器人客户端。
type DingTalk struct {
	URL       string
	Prefix    string
	MaxPerMin int
	client    *http.Client
	lastSent  time.Time
}

// NewDingTalk 创建客户端。
func NewDingTalk(url, prefix string, maxPerMin int) *DingTalk {
	if maxPerMin <= 0 {
		maxPerMin = 15
	}
	return &DingTalk{
		URL:       url,
		Prefix:    prefix,
		MaxPerMin: maxPerMin,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Interval 返回两条消息之间必须保持的最小间隔（钉钉限流 20 条/分钟）。
func (d *DingTalk) Interval() time.Duration {
	iv := time.Minute / time.Duration(d.MaxPerMin)
	if iv < 2*time.Second {
		iv = 2 * time.Second
	}
	return iv
}

type textPayload struct {
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
}

type dingResp struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// Send 发送一条文本消息。钉钉正文上限较宽，这里仍按 1900 字节截断保平安。
func (d *DingTalk) Send(content string) error {
	if strings.TrimSpace(d.URL) == "" {
		return fmt.Errorf("钉钉 webhook 未配置")
	}

	// 与上一条保持间隔，避免触发限流
	if !d.lastSent.IsZero() {
		if wait := d.Interval() - time.Since(d.lastSent); wait > 0 {
			time.Sleep(wait)
		}
	}

	runes := []rune(content)
	if len(runes) > 1900 {
		content = string(runes[:1900])
	}

	var p textPayload
	p.MsgType = "text"
	p.Text.Content = content

	body, err := json.Marshal(p)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求钉钉失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("读取钉钉响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("钉钉返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}

	var r dingResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("钉钉响应非 JSON: %s", truncate(string(raw), 160))
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("钉钉错误 %d: %s", r.ErrCode, r.ErrMsg)
	}

	d.lastSent = time.Now()
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
