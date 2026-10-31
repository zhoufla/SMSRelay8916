// Package modem 封装与 ModemManager 的交互。
//
// 数据读取走 mmcli 的 -J（JSON）输出：这是早期 Python 版反复验证过的路径，
// 枚举值已经是字符串（deliver / received / submit ...），不需要我们自己去
// 映射 D-Bus 的原始 uint32 —— 那些映射一旦搞错，就会把"接收中"误判成"已接收"，
// 结果在长短信分片还没到齐时就把对象删掉，造成永久丢短信。
//
// 事件通知走 D-Bus 信号（org.freedesktop.ModemManager1.Modem.Messaging 的
// Added / Deleted），替代"每 3 秒 spawn 一次 mmcli"的轮询 —— 这是 Go 版相对
// Python 版最主要的收益：常驻 CPU 从约 2% 降到接近 0。
package modem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Client 是 ModemManager 的轻量客户端。
type Client struct {
	Index string // mmcli -m <Index>，通常是 "0"
}

// New 创建客户端。
func New(index string) *Client {
	if index == "" {
		index = "0"
	}
	return &Client{Index: index}
}

// run 执行 mmcli，返回 stdout。
func (c *Client) run(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "mmcli", args...)
	// 固定 locale，避免不同环境下字段名/枚举字符串变化
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")

	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("mmcli 超时(%s): %s", timeout, strings.Join(args, " "))
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("mmcli 失败: %s", msg)
	}
	return string(out), nil
}

// runJSON 执行 mmcli 并把输出解析进 v。
func (c *Client) runJSON(v interface{}, timeout time.Duration, args ...string) error {
	out, err := c.run(timeout, args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("mmcli 输出为空")
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("解析 mmcli JSON 失败: %w", err)
	}
	return nil
}

// SMS 是一条短信的解析结果。
type SMS struct {
	Path      string
	Number    string
	Text      string
	PDUType   string
	State     string
	Timestamp string
	Storage   string
	SMSC      string
	Reference string
	RawText   string // 未做 "[非文本内容]" 兜底前的原始值
}

type smsEnvelope struct {
	SMS struct {
		Content    map[string]interface{} `json:"content"`
		Properties map[string]interface{} `json:"properties"`
	} `json:"sms"`
}

func getString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// isNotFound 判断错误是否表示「目标对象已不存在」。
//
// ModemManager 在不同阶段的措辞完全不同：对象被删除时是
// "couldn't find SMS at ..."，modem 正在重新枚举时是 "no modems found"。
// 字面不稳定，所以统一按小写关键字匹配；匹配不上就当成真实错误让调用方退避重试。
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{
		"not found",
		"couldn't find",
		"cannot find",
		"no modems found",
		"unknown object",
		"no such",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// Fetch 读取单条短信。对象已不存在时返回 (nil, nil)。
func (c *Client) Fetch(path string) (*SMS, error) {
	var env smsEnvelope
	err := c.runJSON(&env, 20*time.Second, "-s", path, "-J")
	if err != nil {
		// 对象已失效属于正常竞争（MM 会删除、modem 会重新枚举），不向上抛
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	content := env.SMS.Content
	props := env.SMS.Properties

	return &SMS{
		Path:      path,
		Number:    strings.TrimSpace(getString(content, "number")),
		Text:      getString(content, "text"),
		RawText:   getString(content, "text"),
		PDUType:   getString(props, "pdu-type"),
		State:     getString(props, "state"),
		Timestamp: getString(props, "timestamp"),
		Storage:   getString(props, "storage"),
		SMSC:      getString(props, "smsc"),
		Reference: getString(props, "message-reference"),
	}, nil
}

// smsPathPattern 用于从 mmcli 的文本输出里抓 SMS 对象路径。
const smsPathPrefix = "/org/freedesktop/ModemManager1/SMS/"

// List 返回当前所有短信对象路径。
func (c *Client) List() ([]string, error) {
	out, err := c.run(20*time.Second, "-m", c.Index, "--messaging-list-sms")
	if err != nil {
		return nil, err
	}
	var out2 []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, smsPathPrefix)
		if i < 0 {
			continue
		}
		rest := line[i+len(smsPathPrefix):]
		// 只取数字部分
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j == 0 {
			continue
		}
		out2 = append(out2, smsPathPrefix+rest[:j])
	}
	return out2, nil
}

// Delete 删除 modem 内的短信副本（释放 73 个槽位之一）。
func (c *Client) Delete(path string) error {
	_, err := c.run(20*time.Second, "-m", c.Index, "--messaging-delete-sms="+path)
	return err
}

// Create 在 modem 内创建一条待发短信，返回对象路径。
//
// 正文必须走临时文件（--messaging-create-sms-with-text）：
// mmcli 的 --messaging-create-sms 用逗号分隔 key=value，正文里只要有逗号
// 就会把参数解析炸掉，而且换行、引号都不可靠。
func (c *Client) Create(number, body string) (string, error) {
	f, err := os.CreateTemp("", "smsbody-*.txt")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return "", fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	out, err := c.run(30*time.Second, "-m", c.Index,
		"--messaging-create-sms=number="+number,
		"--messaging-create-sms-with-text="+tmp)
	if err != nil {
		return "", err
	}
	i := strings.Index(out, smsPathPrefix)
	if i < 0 {
		return "", fmt.Errorf("创建短信失败，mmcli 未返回对象路径: %s", strings.TrimSpace(out))
	}
	rest := out[i+len(smsPathPrefix):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return "", fmt.Errorf("创建短信失败，对象路径异常")
	}
	return smsPathPrefix + rest[:j], nil
}

// Send 发送已创建的短信对象。
func (c *Client) Send(path string) error {
	_, err := c.run(30*time.Second, "-m", c.Index, "-s", path, "--send")
	return err
}

// Status 是状态面板需要的信息。
type Status struct {
	Online        bool        `json:"online"`
	State         string      `json:"state"`
	AccessTech    string      `json:"access_tech"`
	Signal        interface{} `json:"signal"`
	Operator      string      `json:"operator"`
	OperatorCode  string      `json:"operator_code"`
	Registration  string      `json:"registration"`
	PSState       string      `json:"ps_state"`
	IMEI          string      `json:"imei"`
	Model         string      `json:"model"`
	Firmware      string      `json:"firmware"`
	IMSISim       string      `json:"imsi"`
	ICCID         string      `json:"iccid"`
	NumberFromSIM string      `json:"own_number"`
}

type modemEnvelope struct {
	Modem struct {
		Generic  map[string]interface{} `json:"generic"`
		ThreeGPP map[string]interface{} `json:"3gpp"`
	} `json:"modem"`
}

type simEnvelope struct {
	SIM struct {
		Properties map[string]interface{} `json:"properties"`
	} `json:"sim"`
}

// Status 读取 modem 与 SIM 的当前状态。
func (c *Client) Status() *Status {
	st := &Status{
		State: "--", AccessTech: "--", Operator: "--", OperatorCode: "--",
		Registration: "--", PSState: "--", IMEI: "--", Model: "--",
		Firmware: "--", IMSISim: "--", ICCID: "--", NumberFromSIM: "--",
	}

	var env modemEnvelope
	if err := c.runJSON(&env, 20*time.Second, "-m", c.Index, "-J"); err != nil {
		return st
	}
	g := env.Modem.Generic
	t := env.Modem.ThreeGPP
	st.Online = true

	if v := getString(g, "state"); v != "" {
		st.State = v
	}
	if v := g["access-technologies"]; v != nil {
		if arr, ok := v.([]interface{}); ok {
			var techs []string
			for _, it := range arr {
				if s, ok := it.(string); ok {
					techs = append(techs, s)
				}
			}
			if len(techs) > 0 {
				st.AccessTech = strings.Join(techs, ",")
			}
		}
	}
	if sq, ok := g["signal-quality"].(map[string]interface{}); ok {
		st.Signal = sq["value"]
	}
	if v := getString(g, "model"); v != "" {
		st.Model = v
	}
	if v := getString(g, "revision"); v != "" {
		st.Firmware = v
	} else if v := getString(g, "firmware-revision"); v != "" {
		st.Firmware = v
	}
	if v, ok := g["own-numbers"].([]interface{}); ok {
		var nums []string
		for _, it := range v {
			if s, ok := it.(string); ok {
				nums = append(nums, s)
			}
		}
		if len(nums) > 0 {
			st.NumberFromSIM = strings.Join(nums, ",")
		}
	}
	if v := getString(t, "operator-name"); v != "" {
		st.Operator = v
	}
	if v := getString(t, "operator-code"); v != "" {
		st.OperatorCode = v
	}
	if v := getString(t, "registration-state"); v != "" {
		st.Registration = v
	}
	if v := getString(t, "packet-service-state"); v != "" {
		st.PSState = v
	}
	if v := getString(t, "imei"); v != "" {
		st.IMEI = v
	}

	var sim simEnvelope
	if err := c.runJSON(&sim, 15*time.Second, "-i", "any", "-J"); err == nil {
		if v := getString(sim.SIM.Properties, "imsi"); v != "" {
			st.IMSISim = v
		}
		if v := getString(sim.SIM.Properties, "iccid"); v != "" {
			st.ICCID = v
		}
	}
	return st
}
