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

// Wi-Fi 工作模式。wlan0 同一时刻只能处于其中一种。
const (
	ModeAP  = "ap"  // 热点：设备发 SSID，其它终端连进来（ipv4 shared = NAT+DHCP）
	ModeSTA = "sta" // 客户端：设备连接家里/上游 Wi-Fi 上网
)

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
	Mode   string `json:"mode"` // "ap" / "sta"（802-11-wireless.mode，空值按 sta 理解）
}

// Status 是当前 Wi-Fi 连接状态。
type Status struct {
	Mode   string `json:"mode"`   // ModeAP / ModeSTA / ""（wlan0 无活跃 Wi-Fi 连接）
	SSID   string `json:"ssid"`   // AP 模式 = 热点 SSID；STA = 上游 SSID
	Signal int    `json:"signal"` // 仅 STA 有意义；AP 模式恒为 0
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

// Status 返回当前连接。未连接任何 Wi-Fi 时 Mode/SSID 为空。
//
// 不走扫描：AP 模式下 wcn36xx 扫描只能看到自己的 beacon（实测），
// 所以直接问 NM 哪个 profile 活跃、它是什么模式。
func (c *Client) Status() (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{}
	name, mode, ssid, err := c.activeLocked()
	if err != nil || name == "" {
		return st, err
	}
	st.Mode, st.SSID = mode, ssid
	st.IP = c.ifaceIPv4()
	if mode == ModeSTA {
		// 信号强度只有客户端模式有：从 NM 的扫描缓存里找活跃 SSID
		list, err := c.scanLines()
		if err == nil {
			for _, n := range list {
				if n.Active && n.SSID == st.SSID {
					st.Signal = n.Signal
					break
				}
			}
		}
	}
	return st, nil
}

// normMode 把 NM 的 mode 值规范化为 ModeSTA/ModeAP。
// NM 里客户端模式叫 infrastructure（且常为空缺省值）。
func normMode(m string) string {
	m = strings.TrimSpace(m)
	if m == ModeAP {
		return ModeAP
	}
	return ModeSTA
}

// connMode 返回 profile 的模式（nmcli -g 直接吐值，无需解析字段名）。
func (c *Client) connMode(profile string) string {
	m, err := c.run("-g", "802-11-wireless.mode", "connection", "show", profile)
	if err != nil {
		return ModeSTA
	}
	return normMode(m)
}

// activeLocked 返回 wlan0 上活跃 Wi-Fi 连接的 profile 名、模式与 SSID。
// 注意 profile 名和 SSID 是两回事（热点 profile 叫 "WIFI"、SSID 是 4G-WIFI），
// 展示一律用 SSID 字段。
func (c *Client) activeLocked() (name, mode, ssid string, err error) {
	out, err := c.run("-g", "GENERAL.CONNECTION", "dev", "show", c.iface)
	if err != nil {
		return "", "", "", err
	}
	line := strings.TrimSpace(out)
	if line == "" || line == "--" {
		return "", "", "", nil
	}
	// 活跃的不是 Wi-Fi profile（usb0/modem 等）时，这两条会报错，按无 Wi-Fi 处理
	modeOut, err := c.run("-g", "802-11-wireless.mode", "connection", "show", line)
	if err != nil {
		return "", "", "", nil
	}
	ssidOut, err := c.run("-g", "802-11-wireless.ssid", "connection", "show", line)
	if err != nil {
		return "", "", "", nil
	}
	return line, normMode(modeOut), strings.TrimSpace(ssidOut), nil
}

// ifaceIPv4 读 wlan0 的 IPv4 地址（Status 的 IP 字段）。
func (c *Client) ifaceIPv4() string {
	out, err := c.run("-g", "IP4.ADDRESS", "dev", "show", c.iface)
	if err != nil {
		return ""
	}
	// 可能多地址，取第一个（形如 192.168.10.208/24）
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// Saved 返回已保存的 Wi-Fi profile 列表（含模式）。
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
		p := Profile{Name: f[0], Active: f[2] == c.iface, Mode: c.connMode(f[0])}
		list = append(list, p)
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

// ---- 模式切换（AP / 客户端） ----
//
// 单 wlan0 只能一种模式，切换 = "谁持有 autoconnect"：
// 目标模式的 profile 设 autoconnect yes、其余 no。这样 NM 重启/设备重启后
// 会自动进到用户最后选择的模式，无需额外状态文件。
//
// 防失联：切换必然断开当前管理路径（切 AP 断家里网络、切 STA 断热点），
// 但 LTE 侧 wwan0 不受影响，smsrelay 的短信业务不中断；切换失败时
// 原 profile 的 autoconnect 语义仍在，NM 会自动回连。

// apProfileLocked 返回第一个 mode=ap 的 profile 名；没有则返回 ""。
func (c *Client) apProfileLocked() (string, error) {
	out, err := c.run("-t", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) < 2 || f[1] != "802-11-wireless" {
			continue
		}
		if c.connMode(f[0]) == ModeAP {
			return f[0], nil
		}
	}
	return "", nil
}

// SetAP 切到热点模式。ssid/password/channel 为空时沿用现有 AP profile 的值。
func (c *Client) SetAP(ssid, password string, channel int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	prof, err := c.apProfileLocked()
	if err != nil {
		return err
	}
	if prof == "" {
		// 没有现成热点 profile，新建（ipv4 shared = NAT + DHCP，客户端可直接上网）
		prof = "WIFI"
		args := []string{"connection", "add", "type", "wifi", "ifname", c.iface,
			"con-name", prof,
			"802-11-wireless.mode", ModeAP,
			"802-11-wireless.band", "bg",
			"ipv4.method", "shared"}
		if ssid != "" {
			args = append(args, "802-11-wireless.ssid", ssid)
		}
		if password != "" {
			args = append(args, "802-11-wireless-security.key-mgmt", "wpa-psk",
				"802-11-wireless-security.psk", password)
		}
		if channel > 0 {
			args = append(args, "802-11-wireless.channel", strconv.Itoa(channel))
		}
		if _, err := c.run(args...); err != nil {
			return fmt.Errorf("创建热点配置失败: %w", err)
		}
	} else {
		// 已有热点 profile：按参数更新（留空沿用原值）
		if ssid != "" {
			_, err = c.run("connection", "modify", prof, "802-11-wireless.ssid", ssid)
		}
		if err == nil && password != "" {
			_, err = c.run("connection", "modify", prof,
				"802-11-wireless-security.key-mgmt", "wpa-psk",
				"802-11-wireless-security.psk", password)
		}
		if err == nil && channel > 0 {
			_, err = c.run("connection", "modify", prof, "802-11-wireless.channel", strconv.Itoa(channel))
		}
		if err != nil {
			return fmt.Errorf("更新热点配置失败: %w", err)
		}
	}

	// autoconnect 归属：AP yes，STA 全部 no
	if err := c.setAutoconnectLocked(ModeAP); err != nil {
		return err
	}

	// 断开当前连接再拉起热点（dev disconnect 保证 STA/Wi-Fi 路径干净退出）
	_, _ = c.run("dev", "disconnect", c.iface)
	time.Sleep(time.Second)
	if _, err := c.run("connection", "up", prof); err != nil {
		return fmt.Errorf("热点启动失败: %w", err)
	}
	return nil
}

// SetSTA 切回客户端模式。优先回连最近保存的客户端网络；
// 一个都没保存时返回 ErrNoSTA，由前端引导用户去扫描连接。
func (c *Client) SetSTA() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.setAutoconnectLocked(ModeSTA); err != nil {
		return err
	}

	// 断开热点
	_, _ = c.run("dev", "disconnect", c.iface)
	time.Sleep(time.Second)

	// 回连第一个（也是唯一场景下的）客户端 profile
	out, err := c.run("-t", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) < 2 || f[1] != "802-11-wireless" {
			continue
		}
		if c.connMode(f[0]) == ModeAP {
			continue
		}
		if _, err := c.run("connection", "up", f[0]); err != nil {
			// 这个网络连不上（信号没了/密码变了），试下一个
			continue
		}
		return nil
	}
	return ErrNoSTA
}

// ErrNoSTA 切回客户端模式时没有任何可用的已保存网络。
var ErrNoSTA = errors.New("没有已保存的客户端 Wi-Fi，请在扫描列表中选择网络连接")

// setAutoconnectLocked 把目标模式的 profile 设为 autoconnect yes、其余 no。
func (c *Client) setAutoconnectLocked(target string) error {
	out, err := c.run("-t", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimSpace(line))
		if len(f) < 2 || f[1] != "802-11-wireless" {
			continue
		}
		mode := c.connMode(f[0])
		want := "no"
		if mode == target {
			want = "yes"
		}
		_, _ = c.run("connection", "modify", f[0], "connection.autoconnect", want)
	}
	return nil
}

