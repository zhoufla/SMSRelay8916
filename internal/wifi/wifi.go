// Package wifi 封装 NetworkManager（nmcli）提供的 Wi-Fi 管理能力。
//
// 设计约束：
//   - 只 shell out 到 nmcli（设备上已有），不引第三方 D-Bus 库 —— 与项目零依赖哲学一致
//   - SSID/密码一律作为 exec.argv 传递，绝不拼接 shell 字符串
//   - 所有命令带 LC_ALL=C：nmcli 的 terse 输出里 yes/no 等字段随 locale 变化
//     （中文环境输出「是/否」，直接解析会瞎），固定 C locale 才能稳定解析
//   - 所有 nmcli 调用经互斥锁串行化：单 wlan0 上并发扫描/连接会互相干扰
//   - 安全底线：任何操作都不删除「当前活跃连接」的 profile —— 连接新网络失败时
//     NM 会按原 profile 自动回连，设备永远有退路（删除活跃 profile 才会自锁）
package wifi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrActiveProfile 尝试删除当前连接的 Wi-Fi profile 时返回。
// API 层据此返回 409。
var ErrActiveProfile = errors.New("不能删除当前连接的配置")

// Network 是扫描发现的一个 Wi-Fi 网络。
type Network struct {
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"`   // 0-100
	Security string `json:"security"` // 如 "WPA2"，空 = 开放
	Active   bool   `json:"active"`
}

// Profile 是一个已保存的 Wi-Fi 连接配置。
type Profile struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// Status 是当前 Wi-Fi 连接状态。
type Status struct {
	SSID   string `json:"ssid"`
	Signal int    `json:"signal"`
	IP     string `json:"ip"`
}

// Client 是对 nmcli 的封装。
type Client struct {
	mu      sync.Mutex
	iface   string
	timeout time.Duration
}

// New 创建 Client。iface 通常是 wlan0。
func New(iface string) *Client {
	return &Client{iface: iface, timeout: 50 * time.Second}
}

// run 执行一条 nmcli 命令并返回 stdout。
//
// locale 用 C.UTF-8 而不是 C：纯 C 下 nmcli 会把 SSID 里的非 ASCII 字符
// 替换成 '?'（「sjj的网络」变「sjj???」），C.UTF-8 既保留英文的 yes/no
// 字段输出，又不破坏 UTF-8 SSID。
func (c *Client) run(args ...string) (string, error) {
	if _, err := exec.LookPath("nmcli"); err != nil {
		return "", errors.New("nmcli 不可用（设备上没有 NetworkManager？）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nmcli", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("nmcli 超时: %s", args[0])
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return out.String(), nil
}

// splitTerse 按 ':' 切分 nmcli terse 输出的一行。
// nmcli 会把值里的 ':' 转义成 '\:'，所以要带转义感知地切。
func splitTerse(line string) []string {
	var out []string
	var cur strings.Builder
	esc := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case esc:
			cur.WriteByte(ch)
			esc = false
		case ch == '\\':
			esc = true
		case ch == ':':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	out = append(out, cur.String())
	return out
}

// scanLines 执行扫描并解析。NM 会缓存扫描结果，通常 1~2 秒内返回。
func (c *Client) scanLines() ([]Network, error) {
	out, err := c.run("-t", "-f", "SSID,SIGNAL,SECURITY,ACTIVE", "dev", "wifi", "list", "ifname", c.iface)
	if err != nil {
		return nil, err
	}
	best := map[string]*Network{}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) < 4 || f[0] == "" {
			continue // 空字段首列是隐藏网络，不展示
		}
		sig, _ := strconv.Atoi(f[1])
		n := Network{SSID: f[0], Signal: sig, Security: f[2], Active: f[3] == "yes"}
		if old, ok := best[n.SSID]; !ok || n.Signal > old.Signal {
			best[n.SSID] = &n // 同 SSID 多 BSS 只留最强
		}
	}
	list := make([]Network, 0, len(best))
	for _, n := range best {
		list = append(list, *n)
	}
	// 信号强的排前面，界面不用再排
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Signal > list[j-1].Signal; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list, nil
}

// Scan 返回周边 Wi-Fi 网络列表（按信号降序）。
func (c *Client) Scan() ([]Network, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scanLines()
}

// Status 返回当前连接。未连接任何 Wi-Fi 时 SSID 为空。
func (c *Client) Status() (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{}
	list, err := c.scanLines()
	if err != nil {
		return st, err
	}
	for _, n := range list {
		if n.Active {
			st.SSID, st.Signal = n.SSID, n.Signal
			break
		}
	}
	return st, nil
}

// Saved 返回已保存的 Wi-Fi profile 列表。
func (c *Client) Saved() ([]Profile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.run("-t", "-f", "NAME,TYPE,DEVICE", "connection", "show")
	if err != nil {
		return nil, err
	}
	var list []Profile
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) < 3 || f[1] != "802-11-wireless" {
			continue
		}
		list = append(list, Profile{Name: f[0], Active: f[2] == c.iface})
	}
	return list, nil
}

// Connect 连接到指定网络。password 为空表示开放网络。
//
// 已存在同名 profile 时直接 `con up`（profile 里的密码优先——想换密码应先忘记再连接，
// 否则 nmcli connect 会因为 profile 已存在而走错分支）。
// 连接失败时原活跃 profile 仍完好，NM 自动回连，不会失联。
func (c *Client) Connect(ssid, password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	saved, err := c.savedLocked()
	if err != nil {
		return err
	}
	for _, p := range saved {
		if p.Name == ssid {
			_, err := c.run("connection", "up", ssid)
			return err
		}
	}
	args := []string{"dev", "wifi", "connect", ssid, "ifname", c.iface}
	if password != "" {
		args = append(args, "password", password)
	}
	_, err = c.run(args...)
	if err != nil {
		// 失败的连接会残留一个无效 profile（nmcli 先建再连），不清掉的话
		// 下次重连会走 con up 而永远用不上新密码。best-effort 清理。
		_, _ = c.run("connection", "delete", ssid)
		msg := err.Error()
		if strings.Contains(msg, "Secrets were required") ||
			strings.Contains(msg, "no supplicant") ||
			strings.Contains(msg, "802.1X") {
			return errors.New("密码错误或认证失败")
		}
		if strings.Contains(msg, "No network with SSID") {
			return errors.New("周边没找到该网络，请重新扫描")
		}
		return err
	}
	return nil
}

// savedLocked 是不加锁的 Saved，供 Connect 内部复用。
func (c *Client) savedLocked() ([]Profile, error) {
	out, err := c.run("-t", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return nil, err
	}
	var list []Profile
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) >= 2 && f[1] == "802-11-wireless" {
			list = append(list, Profile{Name: f[0]})
		}
	}
	return list, nil
}

// Forget 删除已保存的 profile。当前活跃连接的 profile 拒绝删除（防自锁）。
func (c *Client) Forget(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 判活跃用 DEVICE==iface；删除前重查一次，避免用过期缓存
	out, err := c.run("-t", "-f", "NAME,TYPE,DEVICE", "connection", "show")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) >= 3 && f[0] == name && f[1] == "802-11-wireless" && f[2] == c.iface {
			return ErrActiveProfile
		}
	}
	_, err = c.run("connection", "delete", name)
	return err
}
