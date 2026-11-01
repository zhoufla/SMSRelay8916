// Package sysinfo 采集本机系统指标（CPU / 内存 / 温度 / 磁盘 / 网络）。
//
// 全部直接读 procfs 与 sysfs，不依赖任何外部命令，也不引入 cgo。
// CPU 使用率和网络速率都需要两次采样做差，所以由后台 goroutine 周期性调用
// Sample() 并把结果缓存下来，HTTP 请求直接读缓存 —— 请求路径零开销。
package sysinfo

import (
	"context"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CPU 是处理器使用情况。
type CPU struct {
	Percent float64    `json:"percent"`
	PerCore []float64  `json:"per_core,omitempty"`
	Cores   int        `json:"cores"`
	FreqMHz int        `json:"freq_mhz,omitempty"`
	Load    [3]float64 `json:"load"`
}

// Mem 是内存与交换分区情况（单位 MB）。
type Mem struct {
	TotalMB     int `json:"total_mb"`
	UsedMB      int `json:"used_mb"`
	AvailableMB int `json:"available_mb"`
	CachedMB    int `json:"cached_mb"`
	SwapTotalMB int `json:"swap_total_mb"`
	SwapUsedMB  int `json:"swap_used_mb"`
}

// Sensor 是单个温度传感器。
type Sensor struct {
	Name string  `json:"name"`
	C    float64 `json:"c"`
}

// Temp 是温度汇总。
//
// 传感器的命名在不同机型上完全不同（本机有 cpu0_1_thermal / gpu_thermal /
// pm8916_thermal），所以按 name 关键字匹配而不是固定索引 —— 换机型不会错位。
type Temp struct {
	CPUC    float64  `json:"cpu_c,omitempty"`
	GPUC    float64  `json:"gpu_c,omitempty"`
	PMICC   float64  `json:"pmic_c,omitempty"`
	MaxC    float64  `json:"max_c"`
	Sensors []Sensor `json:"sensors,omitempty"`
}

// Disk 是磁盘占用。
type Disk struct {
	Path    string  `json:"path"`
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
	FreeGB  float64 `json:"free_gb"`
	Percent float64 `json:"percent"`
}

// NetIface 是单个网络接口的速率。
type NetIface struct {
	RxBps     int64  `json:"rx_bps"`
	TxBps     int64  `json:"tx_bps"`
	RxTotalMB int64  `json:"rx_total_mb,omitempty"`
	TxTotalMB int64  `json:"tx_total_mb,omitempty"`
	IP        string `json:"ip,omitempty"` // 第一个全局 IPv4，无则空
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

// IfaceIPv4 取接口上第一个全局 IPv4（排除回环与链路本地 169.254）。
// 供本包采样与 Wi-Fi 状态共用，无则返回空串。
func IfaceIPv4(name string) string {
	ni, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, _ := ni.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				return ip4.String()
			}
		}
	}
	return ""
}

// Procs 是进程数。
type Procs struct {
	Running int `json:"running"`
	Total   int `json:"total"`
}

// Snapshot 是一次完整的采样结果。
type Snapshot struct {
	CPU     CPU                 `json:"cpu"`
	Mem     Mem                 `json:"mem"`
	Temp    Temp                `json:"temp"`
	Disk    Disk                `json:"disk"`
	Net     map[string]NetIface `json:"net"`
	UptimeS int64               `json:"uptime_s"`
	Procs   Procs               `json:"procs"`
	TS      string              `json:"ts"`
}

// cpuTimes 是 /proc/stat 里一行 cpu 的累计时间片（单位 jiffies）。
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (t cpuTimes) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

func (t cpuTimes) idleAll() uint64 { return t.idle + t.iowait }

type netCount struct {
	rx, tx uint64
}

// Collector 负责周期性采样并缓存结果。
type Collector struct {
	mu        sync.RWMutex
	prevCPU   map[string]cpuTimes
	prevNet   map[string]netCount
	prevAt    time.Time
	last      *Snapshot
	netIfaces []string
}

// New 创建采集器。netIfaces 是要统计流量的接口名。
func New(netIfaces ...string) *Collector {
	if len(netIfaces) == 0 {
		netIfaces = []string{"wlan0", "wwan0"}
	}
	return &Collector{
		prevCPU:   make(map[string]cpuTimes),
		prevNet:   make(map[string]netCount),
		netIfaces: netIfaces,
	}
}

// Last 返回最近一次采样结果（可能为 nil，表示还没采过）。
func (c *Collector) Last() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

// Start 启动后台采样循环。首次会先采样一次建立基线，之后每 interval 一次。
func (c *Collector) Start(ctx context.Context, interval time.Duration, onChange func(*Snapshot)) {
	// 建立基线：这一次的 CPU 差值没有意义，丢弃
	_, _ = c.Sample()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s, err := c.Sample()
			if err != nil || s == nil {
				continue
			}
			if onChange != nil {
				onChange(s)
			}
		}
	}
}

// Sample 采集一次。首次调用时 CPU 百分比与网络速率为 0（没有基线）。
func (c *Collector) Sample() (*Snapshot, error) {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	s := &Snapshot{
		TS:  now.Format(time.RFC3339),
		Net: make(map[string]NetIface),
	}

	s.CPU, s.Procs = c.readCPU(now)
	s.Mem = readMem()
	s.Temp = readTemp()
	s.Disk = readDisk("/")
	s.Net = c.readNet(now)
	s.UptimeS = readUptime()

	c.prevAt = now
	c.last = s
	return s, nil
}

// readCPU 读 /proc/stat 并按与上次采样的差值算使用率。
func (c *Collector) readCPU(now time.Time) (CPU, Procs) {
	var out CPU
	out.Cores = 1

	f, err := os.Open("/proc/stat")
	if err != nil {
		return out, Procs{}
	}
	defer f.Close()

	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	lines := strings.Split(string(buf[:n]), "\n")

	var allTimes cpuTimes
	haveAll := false
	var perCore []float64
	cores := 0

	for _, line := range lines {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		name := fields[0]
		t := parseCPUTimes(fields[1:])

		if name == "cpu" {
			allTimes = t
			haveAll = true
			continue
		}
		// cpu0, cpu1, ...
		if _, err := strconv.Atoi(name[3:]); err != nil {
			continue
		}
		cores++
		if prev, ok := c.prevCPU[name]; ok {
			perCore = append(perCore, usagePercent(prev, t))
		}
		c.prevCPU[name] = t
	}

	if cores > 0 {
		out.Cores = cores
	}
	out.PerCore = perCore

	if haveAll {
		if prev, ok := c.prevCPU["cpu"]; ok {
			out.Percent = usagePercent(prev, allTimes)
		}
		c.prevCPU["cpu"] = allTimes
	}

	// 负载与进程数
	if l1, l5, l15, running, total, ok := readLoadavg(); ok {
		out.Load = [3]float64{l1, l5, l15}
		_ = running
		_ = total
	}

	if khz := readIntFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq"); khz > 0 {
		out.FreqMHz = int(khz / 1000)
	}

	return out, readProcs()
}

func parseCPUTimes(fields []string) cpuTimes {
	get := func(i int) uint64 {
		if i >= len(fields) {
			return 0
		}
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		return v
	}
	return cpuTimes{
		user: get(0), nice: get(1), system: get(2), idle: get(3),
		iowait: get(4), irq: get(5), softirq: get(6), steal: get(7),
	}
}

func usagePercent(prev, cur cpuTimes) float64 {
	dt := float64(cur.total()) - float64(prev.total())
	if dt <= 0 {
		return 0
	}
	di := float64(cur.idleAll()) - float64(prev.idleAll())
	p := (dt - di) / dt * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return round1(p)
}

// readMem 读 /proc/meminfo。
func readMem() Mem {
	var m Mem
	vals := map[string]int{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()

	scan := make([]byte, 16384)
	n, _ := f.Read(scan)
	for _, line := range strings.Split(string(scan[:n]), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := line[:i]
		rest := strings.Fields(line[i+1:])
		if len(rest) == 0 {
			continue
		}
		v, _ := strconv.Atoi(rest[0])
		vals[key] = v // kB
	}

	toMB := func(kb int) int { return kb / 1024 }
	m.TotalMB = toMB(vals["MemTotal"])
	m.AvailableMB = toMB(vals["MemAvailable"])
	m.CachedMB = toMB(vals["Cached"] + vals["Buffers"])
	m.UsedMB = m.TotalMB - m.AvailableMB
	if m.UsedMB < 0 {
		m.UsedMB = 0
	}
	m.SwapTotalMB = toMB(vals["SwapTotal"])
	m.SwapUsedMB = toMB(vals["SwapTotal"] - vals["SwapFree"])
	return m
}

// readTemp 读所有 hwmon 传感器。毫摄氏度 → 摄氏度。
//
// hwmon 路径依赖内核把 tsens 注册成 hwmon 设备（该支持 5.19 才合入 mainline，
// jz02v10 的 6.17 内核有）。5.15 一类的老内核只有 thermal class
// （/sys/class/thermal/thermal_zone*，如 SP970 的 cpu0-1-thermal / modem-thermal），
// 所以 hwmon 什么都没读到时，回退扫 thermal class，用 zone type 当传感器名。
// 两个来源不会同时生效，不会重复计数。
func readTemp() Temp {
	var t Temp
	paths, _ := filepath.Glob("/sys/class/hwmon/hwmon*/temp1_input")
	for _, p := range paths {
		dir := filepath.Dir(p)
		name := strings.TrimSpace(readStringFile(filepath.Join(dir, "name")))
		if name == "" {
			name = filepath.Base(dir)
		}
		raw := readIntFile(p)
		if raw == 0 {
			continue
		}
		c := round1(float64(raw) / 1000.0)
		t.Sensors = append(t.Sensors, Sensor{Name: name, C: c})
		if c > t.MaxC {
			t.MaxC = c
		}
		switch {
		case strings.Contains(name, "cpu"):
			if c > t.CPUC || t.CPUC == 0 {
				t.CPUC = c
			}
		case strings.Contains(name, "gpu"):
			t.GPUC = c
		case strings.Contains(name, "pm8916") || strings.Contains(name, "pmic"):
			t.PMICC = c
		}
	}
	if len(t.Sensors) == 0 {
		paths, _ = filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
		for _, p := range paths {
			dir := filepath.Dir(p)
			name := strings.TrimSpace(readStringFile(filepath.Join(dir, "type")))
			if name == "" {
				name = filepath.Base(dir)
			}
			raw := readIntFile(p)
			if raw == 0 {
				continue
			}
			c := round1(float64(raw) / 1000.0)
			t.Sensors = append(t.Sensors, Sensor{Name: name, C: c})
			if c > t.MaxC {
				t.MaxC = c
			}
			switch {
			case strings.Contains(name, "cpu"):
				if c > t.CPUC || t.CPUC == 0 {
					t.CPUC = c
				}
			case strings.Contains(name, "gpu"):
				t.GPUC = c
			case strings.Contains(name, "pm8916") || strings.Contains(name, "pmic"):
				t.PMICC = c
			}
		}
	}
	return t
}

// readDisk 用 Statfs 取挂载点容量。
func readDisk(path string) Disk {
	d := Disk{Path: path}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return d
	}
	bs := float64(st.Bsize)
	total := float64(st.Blocks) * bs
	free := float64(st.Bavail) * bs
	used := total - float64(st.Bfree)*bs
	d.TotalGB = round1(total / (1 << 30))
	d.UsedGB = round1(used / (1 << 30))
	d.FreeGB = round1(free / (1 << 30))
	if total > 0 {
		d.Percent = round1(used / total * 100)
	}
	return d
}

// readNet 读接口计数器并算速率。
func (c *Collector) readNet(now time.Time) map[string]NetIface {
	out := make(map[string]NetIface, len(c.netIfaces))

	var dt float64
	if !c.prevAt.IsZero() {
		dt = now.Sub(c.prevAt).Seconds()
	}

	for _, name := range c.netIfaces {
		base := filepath.Join("/sys/class/net", name)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		rx := uint64(readIntFile(filepath.Join(base, "statistics", "rx_bytes")))
		tx := uint64(readIntFile(filepath.Join(base, "statistics", "tx_bytes")))

		iface := NetIface{
			Available: true,
			RxTotalMB: int64(rx / (1 << 20)),
			TxTotalMB: int64(tx / (1 << 20)),
			IP:        IfaceIPv4(name),
		}

		// 某些驱动（本机的 wwan0）不实现字节计数，恒为 0。
		// 这时标记为不可用，而不是显示一个假的 0 速率。
		if rx == 0 && tx == 0 {
			iface.Available = false
			iface.Note = "驱动未实现流量统计"
		}

		if prev, ok := c.prevNet[name]; ok && dt > 0 {
			drx := float64(rx) - float64(prev.rx)
			dtx := float64(tx) - float64(prev.tx)
			if drx >= 0 {
				iface.RxBps = int64(drx / dt)
			}
			if dtx >= 0 {
				iface.TxBps = int64(dtx / dt)
			}
		}
		c.prevNet[name] = netCount{rx: rx, tx: tx}
		out[name] = iface
	}
	return out
}

func readUptime() int64 {
	s := strings.TrimSpace(readStringFile("/proc/uptime"))
	if s == "" {
		return 0
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	f, _ := strconv.ParseFloat(fields[0], 64)
	return int64(f)
}

func readLoadavg() (l1, l5, l15 float64, running, total int, ok bool) {
	s := strings.TrimSpace(readStringFile("/proc/loadavg"))
	fields := strings.Fields(s)
	if len(fields) < 4 {
		return
	}
	l1, _ = strconv.ParseFloat(fields[0], 64)
	l5, _ = strconv.ParseFloat(fields[1], 64)
	l15, _ = strconv.ParseFloat(fields[2], 64)
	if parts := strings.SplitN(fields[3], "/", 2); len(parts) == 2 {
		running, _ = strconv.Atoi(parts[0])
		total, _ = strconv.Atoi(parts[1])
	}
	ok = true
	return
}

func readProcs() Procs {
	_, _, _, running, total, _ := readLoadavg()
	return Procs{Running: running, Total: total}
}

func readStringFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func readIntFile(path string) int64 {
	s := strings.TrimSpace(readStringFile(path))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func round1(f float64) float64 {
	return math.Round(f*10) / 10
}
