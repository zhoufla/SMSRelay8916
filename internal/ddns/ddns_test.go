package ddns

import "testing"

func TestJudge(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		ip   string
		want string
		why  string
	}{
		{
			name: "external 的 A 记录是出口 IP，不是本机",
			rec:  Record{Type: "A", Source: "external"},
			ip:   "203.0.113.10",
			want: "nat",
			why:  "出口 IP 属于运营商/路由器，除非端口映射否则不可入站；早期误判为 ok",
		},
		{
			name: "私网 IPv4",
			rec:  Record{Type: "A", Source: "iface:eth0"},
			ip:   "192.168.1.50",
			want: "cgnat",
		},
		{
			name: "运营商级 NAT",
			rec:  Record{Type: "A", Source: "iface:wwan0"},
			ip:   "10.64.0.10",
			want: "cgnat",
		},
		{
			name: "wlan0 的公网 IPv6 可用",
			rec:  Record{Type: "AAAA", Source: "iface:wlan0"},
			ip:   "2001:db8:1234:5678::1",
			want: "ok",
		},
		{
			name: "wwan0 的 IPv6 数据面不通",
			rec:  Record{Type: "AAAA", Source: "iface:wwan0"},
			ip:   "2001:db8:1234:5678::2",
			want: "no-v6",
		},
		{
			name: "链路本地地址不可能是公网记录",
			rec:  Record{Type: "AAAA", Source: "iface:eth0"},
			ip:   "fe80::1",
			want: "cgnat",
			why:  "早期只判断 wwan0，fe80:: 也被标成可达",
		},
		{
			name: "唯一本地地址 fc00::/7",
			rec:  Record{Type: "AAAA", Source: "iface:eth0"},
			ip:   "fd00::1",
			want: "cgnat",
		},
		{
			name: "回环",
			rec:  Record{Type: "A", Source: "iface:lo"},
			ip:   "127.0.0.1",
			want: "cgnat",
		},
		{
			name: "解析不了",
			rec:  Record{Type: "A", Source: "external"},
			ip:   "not-an-ip",
			want: "unknown",
		},
		{
			name: "IPv6 回环地址",
			rec:  Record{Type: "AAAA", Source: "iface:lo"},
			ip:   "::1",
			want: "cgnat",
			why:  "v6 分支早期没判回环，::1 被标成可达",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Judge(c.rec, c.ip)
			if got != c.want {
				t.Fatalf("Judge(%s, %s) = %q, want %q %s", c.rec.Source, c.ip, got, c.want, c.why)
			}
		})
	}
}
