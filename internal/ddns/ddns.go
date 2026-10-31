// Package ddns 把本机地址同步到 Cloudflare DNS。
//
// 设计要点：
//   - 一条记录 = 一个域名 + 一种记录类型（A 或 AAAA），二者完全解耦。
//     同一域名可以建两条，也可以只建一条，由用户按自己的网络环境决定。
//   - 取地址支持两种来源：本地接口（iface:xxx）或外部出口查询（external）。
//   - 只有在地址真的变化时才调用 Cloudflare API。
//   - 附带可达性判断：本机 IPv4 是 CGNAT，A 记录填上去外部也连不进来，
//     这一点必须让用户在界面上直接看到，而不是配完才发现。
package ddns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Record 是一条 DDNS 记录。
type Record struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`    // 完整记录名，如 sub.example.com
	Type       string `json:"type"`    // A | AAAA
	ZoneID     string `json:"zone_id"` // 留空则按域名自动查
	APIToken   string `json:"api_token"`
	Source     string `json:"source"` // iface:wlan0 | iface:wwan0 | external
	Proxied    bool   `json:"proxied"`
	TTL        int    `json:"ttl"`
	Enabled    bool   `json:"enabled"`
	LastIP     string `json:"last_ip"`
	LastUpdate string `json:"last_update"`
	LastError  string `json:"last_error"`

	// 以下字段在查询时实时计算，不落库
	CurrentIP    string `json:"current_ip,omitempty"`
	Reachability string `json:"reachability,omitempty"` // ok | cgnat | no-v6 | unknown
	ReachNote    string `json:"reach_note,omitempty"`
}

// DefaultV4Endpoint 是取出口 IPv4 的默认服务。国内可达。
const DefaultV4Endpoint = "https://api.ipify.org"

// Client 是 Cloudflare API 客户端。
type Client struct {
	HTTP *http.Client
}

// NewClient 创建客户端。
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// cfResp 是 Cloudflare 的统一响应包。
type cfResp struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (r cfResp) errText() string {
	if len(r.Errors) == 0 {
		return "未知错误"
	}
	parts := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		parts = append(parts, fmt.Sprintf("%d:%s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}

func (c *Client) call(method, path, token string, body interface{}, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, "https://api.cloudflare.com/client/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Cloudflare 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var cr cfResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return fmt.Errorf("Cloudflare 响应无法解析 (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 160))
	}
	if !cr.Success {
		return fmt.Errorf("Cloudflare 拒绝 (HTTP %d): %s", resp.StatusCode, cr.errText())
	}
	if out != nil && len(cr.Result) > 0 {
		if err := json.Unmarshal(cr.Result, out); err != nil {
			return fmt.Errorf("解析 result 失败: %w", err)
		}
	}
	return nil
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// findZone 从记录名推断主域名并查询 zone id。
//
// 只尝试「去掉一级」的猜测，不做完整的公共后缀解析 —— .com.cn 这类二级后缀
// 会猜错，所以界面上允许直接填 zone_id 兜底。
func (c *Client) findZone(token, recordName string) (string, error) {
	parts := strings.Split(recordName, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("记录名不合法: %s", recordName)
	}

	// 从最短的后缀开始试，先试 example.com，再试 com
	for i := 1; i < len(parts); i++ {
		guess := strings.Join(parts[i:], ".")
		var zones []cfZone
		if err := c.call(http.MethodGet, "/zones?name="+guess, token, nil, &zones); err != nil {
			continue
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("找不到 %s 对应的 zone，请在记录里直接填 zone_id", recordName)
}

// findRecord 查找同名同类型的记录。
func (c *Client) findRecord(token, zoneID, name, typ string) (*cfRecord, error) {
	var recs []cfRecord
	path := fmt.Sprintf("/zones/%s/dns_records?type=%s&name=%s", zoneID, typ, name)
	if err := c.call(http.MethodGet, path, token, nil, &recs); err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

func (c *Client) createRecord(token, zoneID, name, typ, content string, proxied bool, ttl int) error {
	body := map[string]interface{}{
		"type":    typ,
		"name":    name,
		"content": content,
		"ttl":     ttl,
		"proxied": proxied,
	}
	return c.call(http.MethodPost, "/zones/"+zoneID+"/dns_records", token, body, nil)
}

func (c *Client) updateRecord(token, zoneID, recID, name, typ, content string, proxied bool, ttl int) error {
	body := map[string]interface{}{
		"type":    typ,
		"name":    name,
		"content": content,
		"ttl":     ttl,
		"proxied": proxied,
	}
	return c.call(http.MethodPut, "/zones/"+zoneID+"/dns_records/"+recID, token, body, nil)
}

// Sync 把一条记录的 DNS 指向当前地址，返回是否有变更。
func (c *Client) Sync(rec *Record, v4Endpoint string) (changed bool, newIP string, err error) {
	ip, err := ResolveIP(rec.Source, rec.Type, v4Endpoint)
	if err != nil {
		return false, "", err
	}
	if ip == rec.LastIP {
		return false, ip, nil
	}

	zoneID := rec.ZoneID
	if zoneID == "" {
		zoneID, err = c.findZone(rec.APIToken, rec.Name)
		if err != nil {
			return false, ip, err
		}
	}

	ttl := rec.TTL
	if ttl <= 0 {
		ttl = 1 // 1 = automatic
	}

	existing, err := c.findRecord(rec.APIToken, zoneID, rec.Name, rec.Type)
	if err != nil {
		return false, ip, err
	}
	if existing == nil {
		if err := c.createRecord(rec.APIToken, zoneID, rec.Name, rec.Type, ip, rec.Proxied, ttl); err != nil {
			return false, ip, err
		}
		return true, ip, nil
	}
	if existing.Content == ip {
		return false, ip, nil
	}
	if err := c.updateRecord(rec.APIToken, zoneID, existing.ID, rec.Name, rec.Type, ip, rec.Proxied, ttl); err != nil {
		return false, ip, err
	}
	return true, ip, nil
}

// ResolveIP 按来源取当前地址。
func ResolveIP(source, typ, v4Endpoint string) (string, error) {
	if strings.HasPrefix(source, "iface:") {
		return ifaceIP(strings.TrimPrefix(source, "iface:"), typ)
	}
	if typ == "AAAA" {
		// 外部 IPv6 查询服务在国内基本不可用，直接要求走接口
		return "", fmt.Errorf("IPv6 不支持 external 来源，请改用 iface:xxx")
	}
	if v4Endpoint == "" {
		v4Endpoint = DefaultV4Endpoint
	}
	return externalIP(v4Endpoint)
}

// ifaceIP 从指定网卡取地址。
//
// 优先 /128 那种（DHCPv6 分配的稳定地址），其次 SLAAC 的 /64。
func ifaceIP(name, typ string) (string, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("网卡 %s 不存在: %w", name, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}

	wantV4 := typ == "A"
	var fallback, preferred string

	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if wantV4 {
			if ip.To4() == nil || !ip.IsGlobalUnicast() {
				continue
			}
			fallback = ip.String()
			continue
		}

		// IPv6
		if ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		if isULA(ip) {
			continue
		}
		if ones, _ := ipn.Mask.Size(); ones == 128 && preferred == "" {
			preferred = ip.String()
		} else if fallback == "" {
			fallback = ip.String()
		}
	}

	if preferred != "" {
		return preferred, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("网卡 %s 上没有可用于 %s 记录的地址", name, typ)
}

func isULA(ip net.IP) bool {
	// fc00::/7
	return len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc
}

// externalIP 查询出口地址。
//
// 这类公共服务经常抖动（本机实测 api.ipify.org 会偶发超时），
// 所以重试 3 次、逐步退避；仍失败就明确报错而不是返回空值，
// 免得把一条空白记录写到 Cloudflare 上。
func externalIP(endpoint string) (string, error) {
	var lastErr error
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 700 * time.Millisecond)
		}
		ip, err := fetchIPOnce(endpoint)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("查询出口 IP 连续 3 次失败，可稍后重试或改用 iface 来源: %w", lastErr)
}

func fetchIPOnce(endpoint string) (string, error) {
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Get(endpoint)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(raw))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("返回了非 IP 内容: %s", truncate(ip, 60))
	}
	return ip, nil
}

// Judge 判断某条记录指向的地址在外部是否可达。
//
// 这是必须给用户看的：本机 IPv4 是 CGNAT，A 记录填上去也连不进来；
// 4G 的 IPv6 只下发了地址、数据面不通。
func Judge(rec Record, ip string) (string, string) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "unknown", "无法判断"
	}

	// 回环与未指定地址不分 v4/v6，一律挡掉（::1 曾被误判成"公网 IPv6 可达"）
	if parsed.IsLoopback() || parsed.IsUnspecified() {
		return "cgnat", "这是回环/未指定地址，不能作为公网记录"
	}

	isV4 := parsed.To4() != nil
	if isV4 {
		if isCGNAT(parsed) || parsed.IsPrivate() {
			return "cgnat", "该地址不是公网 IPv4（CGNAT/私网），外部无法连入"
		}
		if rec.Source == "external" {
			// 出口 IP 看起来是公网的，但它属于运营商网关/路由器，不是本机。
			// 除非用户在路由器上做了端口映射，否则外部连不到这台设备。
			return "nat", "这是出口 IP，不是本机地址；需在路由器上做端口映射才能入站"
		}
		return "ok", "本机公网 IPv4"
	}

	// 链路本地 / 未指定地址连「是不是公网」都谈不上，先挡掉。
	// 早期只判断了 wwan0，导致 eth0 上的 fe80:: 也被标成「公网 IPv6，可达」。
	if parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() || parsed.IsUnspecified() {
		return "cgnat", "这是链路本地地址（fe80::/10），不能作为公网记录"
	}
	// Go 的 IsPrivate 对 IPv6 覆盖 fc00::/7（唯一本地地址）
	if parsed.IsPrivate() {
		return "cgnat", "这是唯一本地地址（fc00::/7），外部无法连入"
	}
	if strings.HasPrefix(rec.Source, "iface:wwan0") {
		return "no-v6", "4G 的 IPv6 只下发地址、数据面不通，外部无法连入"
	}
	return "ok", "公网 IPv6，仍需路由器/防火墙放行入站"
}

// isCGNAT 判断是否落在 100.64.0.0/10（运营商级 NAT）。
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
