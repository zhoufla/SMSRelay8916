// Command smsrelay 是 jz02v10 (MSM8916) 上的短信网关。
//
// 设计要点
// --------
//   - 短信存储只有 73 个槽位（modem ME 23 + SIM 50），在通信处理器里写死，
//     无法扩容。因此采用「收到即取走」：归档到 SQLite → 立即删除 modem 内的副本
//     → 槽位始终为空。
//   - 取：ModemManager 的 D-Bus 信号驱动（Messaging.Added / Deleted），
//     空闲时完全不产生子进程；数据读取仍走 mmcli -J（枚举已是字符串，无需映射）。
//   - 发：正文写入临时文件后走 --messaging-create-sms-with-text，
//     彻底避开 mmcli 对逗号/换行的转义问题。
//   - 转发：SQLite 里的转发队列 + 独立 goroutine，网络抖动不影响收取。
//   - 对外：HTTP API + 内置单页 Web UI，接口与既有 Python 版完全兼容。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zhoufla/SMSRelay8916/internal/api"
	"github.com/zhoufla/SMSRelay8916/internal/modem"
	"github.com/zhoufla/SMSRelay8916/internal/relay"
	"github.com/zhoufla/SMSRelay8916/internal/store"
	"github.com/zhoufla/SMSRelay8916/internal/sysinfo"
)

// Version 是构建版本，编译时可通过 -ldflags "-X main.Version=..." 覆盖。
var Version = "dev"

const (
	// pendingMax：状态一直是未知/中间态时的兜底秒数。
	pendingMax = 30 * time.Second
	// holdSeconds：正文必须稳定这么久才允许归档并删除。
	//
	// 长短信是多条 PDU 分片陆续到达的，MM 会把它们拼进同一个 SMS 对象。
	// 如果在一片刚到、下一片还在路上时就把对象删掉，剩下那片会留在 SIM 里变成孤儿，
	// 随后短信中心复用相同分片序号时就会撞车
	// （MM 日志：A part with index 1 was already taken），整条短信永久丢失。
	holdSeconds = 6 * time.Second
	// requeueDelay：条件未满足时多久后重新检查同一个对象。
	requeueDelay = 2 * time.Second
	// maxFetchFailures：同一个对象读取失败的上限。
	// 超过就放弃等待，交给兜底轮询重新发现 —— 无上限重试会每 2 秒刷一条 WARN，
	// 把日志淹掉，而且掩盖真正的问题。
	maxFetchFailures = 5
)

// pendingStates 里的状态一律不能取走。
//
// 长短信分段到达时 MM 先给出 receiving 且正文为 "--"；发送过程中也会短暂出现
// state="--"（modem 未回报）。此时归档既拿不到内容，又会破坏 MM 的分段重组。
var pendingStates = map[string]bool{
	"sending":   true,
	"receiving": true,
	"draft":     true,
	"unknown":   true,
	"pending":   true,
	"--":        true,
	"":          true,
}

// DingTalkConfig 是钉钉转发配置。
type DingTalkConfig struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url"`
	Prefix    string `json:"prefix"`
	MaxPerMin int    `json:"max_per_min"`
}

// Config 是运行配置。字段名与既有 Python 版保持一致，可直接复用旧配置文件。
type Config struct {
	ListenHost    string         `json:"listen_host"`
	ListenPort    int            `json:"listen_port"`
	APIToken      string         `json:"api_token"`
	DBPath        string         `json:"db_path"`
	PollInterval  int            `json:"poll_interval"`  // 兼容旧配置，Go 版不再使用
	GuardInterval int            `json:"guard_interval"` // 兜底轮询秒数，默认 60
	ModemIndex    string         `json:"modem_index"`
	WebDir        string         `json:"web_dir"`
	OwnNumber     string         `json:"own_number"`
	DingTalk      DingTalkConfig `json:"dingtalk"`
}

func defaultConfig() *Config {
	return &Config{
		ListenHost:    "0.0.0.0",
		ListenPort:    8088,
		DBPath:        "/var/lib/smsrelay/sms.db",
		GuardInterval: 60,
		ModemIndex:    "0",
		WebDir:        "/opt/smsrelay/web",
	}
}

// loadConfig 读取配置。查找顺序：
//  1. -config 参数
//  2. 环境变量 SMSRELAY_CONFIG
//  3. /etc/smsrelay/config.json
//  4. /etc/sms-gw/config.json（兼容早期 Python 版）
func loadConfig(explicit string) (*Config, string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if p := os.Getenv("SMSRELAY_CONFIG"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		"/etc/smsrelay/config.json",
		"/etc/sms-gw/config.json",
	)

	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		cfg := defaultConfig()
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, p, fmt.Errorf("解析配置 %s 失败: %w", p, err)
		}
		applyDefaults(cfg)
		return cfg, p, nil
	}

	cfg := defaultConfig()
	applyDefaults(cfg)
	return cfg, "", nil
}

func applyDefaults(cfg *Config) {
	if cfg.ListenHost == "" {
		cfg.ListenHost = "0.0.0.0"
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = 8088
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "/var/lib/smsrelay/sms.db"
	}
	if cfg.GuardInterval <= 0 {
		cfg.GuardInterval = 60
	}
	if cfg.ModemIndex == "" {
		cfg.ModemIndex = "0"
	}
	if cfg.DingTalk.MaxPerMin <= 0 {
		cfg.DingTalk.MaxPerMin = 15
	}
}

// smsState 记录一个 SMS 对象在本进程内的观察历史。
type smsState struct {
	firstSeen  time.Time // 首次看到该对象的时间，用于 pending 超时兜底
	lastChange time.Time // 正文/状态最后一次变化的时间，用于稳定性判断
	lastSig    string
	failures   int // 连续读取失败次数，用于指数退避与放弃判定
}

// App 把各组件装配在一起，并实现 api.Backend。
type App struct {
	cfg      *Config
	cfgPath  string
	store    *store.Store
	modem    *modem.Client
	relay    *relay.DingTalk
	sys      *sysinfo.Collector
	startTS  time.Time
	hostname string

	events chan string // 待处理的 SMS 对象路径

	mu     sync.Mutex // 保护 states
	states map[string]*smsState

	sendMu sync.Mutex // 发送串行化，避免多路并发挤爆 modem
}

func main() {
	var (
		configFlag = flag.String("config", "", "配置文件路径")
		checkFlag  = flag.Bool("check", false, "仅校验配置与依赖，不启动服务")
		printFlag  = flag.Bool("print-config", false, "打印生效的配置后退出")
		versionFlg = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	if *versionFlg {
		fmt.Printf("smsrelay %s\n", Version)
		return
	}

	cfg, usedPath, err := loadConfig(*configFlag)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if usedPath != "" {
		log.Printf("配置文件: %s", usedPath)
	} else {
		log.Printf("未找到配置文件，使用内置默认值")
	}

	if *printFlag {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("打开归档库失败: %v", err)
	}
	defer st.Close()
	log.Printf("归档库: %s", cfg.DBPath)

	hostname, _ := os.Hostname()
	app := &App{
		cfg:      cfg,
		cfgPath:  usedPath,
		store:    st,
		modem:    modem.New(cfg.ModemIndex),
		sys:      sysinfo.New("wlan0", "wwan0"),
		startTS:  time.Now(),
		hostname: hostname,
		events:   make(chan string, 512),
		states:   make(map[string]*smsState),
	}
	if cfg.DingTalk.Enabled && cfg.DingTalk.URL != "" {
		app.relay = relay.NewDingTalk(cfg.DingTalk.URL, cfg.DingTalk.Prefix, cfg.DingTalk.MaxPerMin)
		log.Printf("钉钉转发: 已启用（上限 %d 条/分钟）", cfg.DingTalk.MaxPerMin)
	} else {
		log.Printf("钉钉转发: 未启用")
	}

	if *checkFlag {
		st0 := app.modem.Status()
		fmt.Printf("配置 OK\n  db      : %s\n  modem   : %s / %s / %s\n  sim     : %s\n",
			cfg.DBPath, st0.State, st0.AccessTech, st0.Registration, st0.IMSISim)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 0) 系统指标采样：5 秒一次，结果缓存，HTTP 请求直接读缓存
	go app.sys.Start(ctx, 5*time.Second, nil)

	// 1) 事件处理 worker（串行，避免同一对象被并发读写）
	go app.worker(ctx)

	// 2) D-Bus 信号订阅；失败则退化为纯轮询
	go app.watchLoop(ctx)

	// 3) 兜底轮询：防止信号丢失导致短信永远躺在 modem 里
	go app.guardLoop(ctx)

	// 4) 转发队列
	if app.relay != nil {
		go app.forwardLoop(ctx)
	}

	// 5) HTTP
	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.ListenPort),
		Handler:           api.New(app, cfg.WebDir).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("HTTP 监听 %s:%d", cfg.ListenHost, cfg.ListenPort)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP 服务异常退出: %v", err)
			cancel()
		}
	}()

	// 启动时立刻做一次全量扫描，把重启前积压的对象收掉。
	// MM 刚起来时可能还没枚举出 modem（"couldn't find modem" / "modem not enabled yet"），
	// 这属于正常竞争，静默重试 30 秒；真失败了再报一次，之后交给兜底轮询。
	go func() {
		for i := 0; i < 15; i++ {
			n, err := app.syncFromModem()
			if err == nil {
				if n > 0 {
					log.Printf("启动扫描: 收到 %d 个待处理对象", n)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		log.Printf("WARN 启动扫描 30 秒内未成功，交给兜底轮询")
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sigCh:
		log.Printf("收到信号 %v，准备退出", s)
	case <-ctx.Done():
	}
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	log.Printf("已退出")
}

// ---------------------------------------------------------------------------
// 事件入口
// ---------------------------------------------------------------------------

func (a *App) enqueue(path string) {
	select {
	case a.events <- path:
	default:
		// 队列满说明处理跟不上，丢掉本次事件；兜底轮询会再捞回来
	}
}

func (a *App) requeue(path string, d time.Duration) {
	time.AfterFunc(d, func() { a.enqueue(path) })
}

// syncFromModem 扫描一次 modem 内的所有对象并入队，返回发现的数量。
//
// 不在这里打日志：启动阶段 ModemManager 尚未枚举完 modem 属于正常竞争，
// 打成 WARN 会让人误以为出故障；是否记录由调用方按场景决定。
func (a *App) syncFromModem() (int, error) {
	paths, err := a.modem.List()
	if err != nil {
		return 0, err
	}
	for _, p := range paths {
		a.enqueue(p)
	}
	return len(paths), nil
}

// handleFetchError 处理读取单个对象失败：指数退避重试，超过上限就放弃。
//
// 放弃是安全的 —— 对象如果真实存在，兜底轮询会重新发现它；
// 而无限重试只会把日志刷满，并且掩盖真正的故障。
func (a *App) handleFetchError(path string, err error) {
	now := time.Now()

	a.mu.Lock()
	rec, ok := a.states[path]
	if !ok {
		rec = &smsState{firstSeen: now, lastChange: now}
		a.states[path] = rec
	}
	rec.failures++
	n := rec.failures
	a.mu.Unlock()

	if n >= maxFetchFailures {
		log.Printf("WARN 读取 %s 连续失败 %d 次，暂时放弃（等兜底轮询重新发现）: %v",
			path, n, err)
		a.mu.Lock()
		delete(a.states, path)
		a.mu.Unlock()
		return
	}

	backoff := time.Duration(1<<uint(n)) * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	log.Printf("WARN 读取 %s 失败(%d/%d)，%s 后重试: %v",
		path, n, maxFetchFailures, backoff, err)
	a.requeue(path, backoff)
}

// worker 串行处理事件队列。
func (a *App) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case path := <-a.events:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("WARN 处理 %s 时 panic: %v", path, r)
					}
				}()
				a.process(path)
			}()
		}
	}
}

// watchLoop 订阅 D-Bus 信号，断线后自动重连。
func (a *App) watchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := a.modem.Watch(ctx, func(ev modem.WatchEvent) {
			if ev.Deleted {
				a.mu.Lock()
				delete(a.states, ev.Path)
				a.mu.Unlock()
				return
			}
			a.enqueue(ev.Path)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("WARN D-Bus 订阅失败(%v)，30 秒后重试；期间依赖兜底轮询", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// guardLoop 周期性全量扫描，兜住漏掉的信号。
func (a *App) guardLoop(ctx context.Context) {
	iv := time.Duration(a.cfg.GuardInterval) * time.Second
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := a.syncFromModem()
			if err != nil {
				log.Printf("WARN 兜底扫描失败: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("兜底扫描: 发现 %d 个待处理对象", n)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 单条短信处理（核心状态机）
// ---------------------------------------------------------------------------

func (a *App) process(path string) {
	sms, err := a.modem.Fetch(path)
	if err != nil {
		a.handleFetchError(path, err)
		return
	}
	if sms == nil {
		// 对象已不存在（被删掉，或 modem 重新枚举），清理状态即可
		a.mu.Lock()
		delete(a.states, path)
		a.mu.Unlock()
		return
	}

	// 状态报告不归档，直接释放槽位
	if sms.PDUType == "delivery-receipt" {
		if err := a.modem.Delete(path); err != nil {
			log.Printf("WARN 删除状态报告 %s 失败: %v", path, err)
		}
		a.mu.Lock()
		delete(a.states, path)
		a.mu.Unlock()
		return
	}

	if sms.PDUType != "deliver" && sms.PDUType != "submit" {
		return
	}

	now := time.Now()

	a.mu.Lock()
	rec, ok := a.states[path]
	if !ok {
		rec = &smsState{firstSeen: now, lastChange: now}
		a.states[path] = rec
	}
	firstSeen := rec.firstSeen
	lastChange := rec.lastChange
	lastSig := rec.lastSig
	a.mu.Unlock()

	state := sms.State
	if pendingStates[state] {
		if now.Sub(firstSeen) < pendingMax {
			// 还没稳定，稍后再看
			a.requeue(path, requeueDelay)
			return
		}
		// 等太久仍是中间态：按方向给个兜底值，避免永远卡在 modem 里
		if sms.PDUType == "deliver" {
			state = "received"
		} else {
			state = "sent"
		}
	}

	body := sms.RawText
	if body == "" || body == "--" {
		body = "[非文本内容]"
	}

	ts := sms.Timestamp
	if ts == "" || strings.HasPrefix(ts, "--") {
		ts = time.Now().Format("2006-01-02T15:04:05-0700")
	}

	// 正文是否已经稳定：分片陆续到达时正文会变长，变一次就重新计时
	sig := state + "|" + body
	if sig != lastSig {
		a.mu.Lock()
		rec.lastSig = sig
		rec.lastChange = now
		a.mu.Unlock()
		a.requeue(path, requeueDelay)
		return
	}
	if now.Sub(lastChange) < holdSeconds {
		a.requeue(path, requeueDelay)
		return
	}

	direction := "in"
	if sms.PDUType == "submit" {
		direction = "out"
	}

	msg := store.Message{
		Dir:     direction,
		Peer:    sms.Number,
		Body:    body,
		TS:      ts,
		State:   state,
		Storage: sms.Storage,
		SMSC:    sms.SMSC,
		PDUType: sms.PDUType,
		MMPath:  path,
	}
	if direction == "in" {
		msg.DelayS = store.DeliveryDelay(ts)
	}

	archived, err := a.store.Archive(msg)
	if err != nil {
		// 入库失败绝不删除，宁可让它留在 modem 里下一轮再试
		log.Printf("WARN 归档失败，保留在原处 %s (%s %s): %v", path, sms.Number, ts, err)
		a.requeue(path, 5*time.Second)
		return
	}
	if !archived {
		log.Printf("WARN 未确认归档，保留在原处 %s (%s %s)", path, sms.Number, ts)
		a.requeue(path, 5*time.Second)
		return
	}

	if err := a.modem.Delete(path); err != nil {
		log.Printf("WARN 删除 %s 失败: %v", path, err)
		a.requeue(path, 5*time.Second)
		return
	}

	a.mu.Lock()
	delete(a.states, path)
	a.mu.Unlock()

	if direction == "in" {
		preview := strings.ReplaceAll(body, "\n", " ")
		r := []rune(preview)
		if len(r) > 40 {
			preview = string(r[:40])
		}
		log.Printf("in  %s %s: %s", sms.Number, ts, preview)
	}
}

// ---------------------------------------------------------------------------
// 发送
// ---------------------------------------------------------------------------

// SendSMS 拆分并逐条发送。
func (a *App) SendSMS(number, text string) api.SendOutcome {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()

	parts := splitBody(text)
	out := api.SendOutcome{Parts: len(parts), Results: make([]api.SendPart, 0, len(parts))}
	allOK := true
	for i, chunk := range parts {
		part := a.sendOne(number, chunk)
		if !part.OK {
			allOK = false
		}
		out.Results = append(out.Results, part)
		if i < len(parts)-1 {
			time.Sleep(time.Second) // 多段之间稍作间隔，避免 modem 忙不过来
		}
	}
	out.OK = allOK
	return out
}

func (a *App) sendOne(number, body string) api.SendPart {
	path, err := a.modem.Create(number, body)
	if err != nil {
		return api.SendPart{OK: false, Error: err.Error()}
	}

	// 先落库占位：这样事件线程看到同一个对象时只会补状态，不会重复入库
	ts := time.Now().Format("2006-01-02T15:04:05-0700")
	if _, err := a.store.Archive(store.Message{
		Dir: "out", Peer: number, Body: body, TS: ts,
		State: "queued", PDUType: "submit", MMPath: path,
	}); err != nil {
		log.Printf("WARN 发送前落库失败: %v", err)
	}

	if err := a.modem.Send(path); err != nil {
		_ = a.store.FailByPath(path, err.Error())
		_ = a.modem.Delete(path)
		return api.SendPart{OK: false, Error: err.Error(), Path: path}
	}

	finalState := map[string]bool{
		"sent": true, "failed": true, "delivered": true, "stored": true,
	}
	state, ref, last := "queued", "", ""
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		sms, err := a.modem.Fetch(path)
		if err != nil || sms == nil {
			break // 对象已被事件线程收走
		}
		if sms.Reference != "" {
			ref = sms.Reference
		}
		last = sms.State
		if finalState[sms.State] {
			state = sms.State
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}

	if state == "queued" {
		// --send 已返回成功、对象也被消费了：按已发送处理
		// （真正的失败 modem 通常会明确给出 failed）
		if last == "failed" {
			state = "failed"
		} else {
			state = "sent"
		}
	}

	_ = a.store.UpdateStateByPath(path, state)
	if st, err := a.store.StateByPath(path); err == nil && st == "failed" {
		state = "failed"
	}
	_ = a.modem.Delete(path)

	return api.SendPart{
		OK:        state == "sent",
		State:     state,
		Reference: ref,
		Path:      path,
	}
}

// GSM-7 基本字符集。全部命中时单条 160 字，否则按 UCS2 单条 70 字。
const gsm7Chars = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

func isGSM7(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune(gsm7Chars, r) {
			return false
		}
	}
	return true
}

// splitBody 把超长正文切成多条，每段尾部加 (i/n) 标记。
func splitBody(body string) []string {
	limit := 160
	if !isGSM7(body) {
		limit = 70
	}
	runes := []rune(body)
	if len(runes) <= limit {
		return []string{body}
	}

	total := (len(runes) + limit - 1) / limit
	out := make([]string, 0, total)
	idx := 0
	for idx < len(runes) {
		marker := fmt.Sprintf("(%d/%d)", len(out)+1, total)
		// 留出标记和它的分隔空格
		take := limit - len([]rune(marker)) - 1
		if take < 1 {
			take = 1
		}
		end := idx + take
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[idx:end])+marker)
		idx = end
	}
	return out
}

// ---------------------------------------------------------------------------
// 转发
// ---------------------------------------------------------------------------

func (a *App) forwardLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := a.store.PendingForward(5)
			if err != nil {
				log.Printf("WARN 读取转发队列失败: %v", err)
				continue
			}
			for _, m := range rows {
				if ctx.Err() != nil {
					return
				}
				if err := a.relay.Send(a.formatForward(m)); err != nil {
					_ = a.store.MarkForwardFailed(m.ID, err.Error())
					log.Printf("forward #%d 失败: %v", m.ID, err)
					continue
				}
				_ = a.store.MarkForwarded(m.ID)
				log.Printf("forwarded #%d from %s", m.ID, m.Peer)
			}
		}
	}
}

func (a *App) formatForward(m store.Message) string {
	ts := m.TS
	if strings.Contains(ts, "T") {
		ts = strings.Replace(ts, "T", " ", 1)
		if len(ts) > 19 {
			ts = ts[:19]
		}
	}
	head := ""
	if prefix := strings.TrimSpace(a.cfg.DingTalk.Prefix); prefix != "" {
		head = prefix + " "
	}
	return fmt.Sprintf("%s收到短信\n号码：%s\n内容：%s\n时间：%s", head, m.Peer, m.Body, ts)
}

// ---------------------------------------------------------------------------
// api.Backend 实现
// ---------------------------------------------------------------------------

// Status 汇总 modem、归档库与网关自身状态。
func (a *App) Status() api.StatusPayload {
	st := a.modem.Status()
	counts, _ := a.store.Counts()
	today, _ := a.store.Today()
	pg, _ := a.store.Paging24h()
	own, src := a.resolveOwnNumber(st)

	return api.StatusPayload{
		Modem:           st,
		OwnNumber:       own,
		OwnNumberSource: src,
		Counts:          counts,
		Today:           today,
		LastInbound:     a.store.LastInbound(),
		Paging:          pg,
		Gateway: api.GatewayInfo{
			Uptime:   int(time.Since(a.startTS).Seconds()),
			Hostname: a.hostname,
			DingTalk: a.relay != nil,
			Version:  Version,
		},
	}
}

// resolveOwnNumber 解析本机号码，返回号码与来源。
//
// 优先级：界面手动设置 > config.json > SIM 卡 > 空。
// SIM 里读不到号码是常态（很多卡不写 EF_MSISDN，本机这张就没有），
// 所以必须允许手动兜底，否则转发内容里永远缺"收信号码"。
func (a *App) resolveOwnNumber(st *modem.Status) (string, string) {
	if v, ok, err := a.store.GetSetting("own_number"); err == nil && ok {
		if v = strings.TrimSpace(v); v != "" {
			return v, "manual"
		}
	}
	if v := strings.TrimSpace(a.cfg.OwnNumber); v != "" {
		return v, "config"
	}
	if st != nil {
		if v := strings.TrimSpace(st.NumberFromSIM); v != "" && v != "--" {
			return v, "sim"
		}
	}
	return "", "none"
}

// System 返回最近一次系统指标采样（由后台 goroutine 每 5 秒刷新）。
func (a *App) System() interface{} {
	return a.sys.Last()
}

// Settings 返回界面可修改的设置项。敏感字段已脱敏。
func (a *App) Settings() interface{} {
	own, src := a.resolveOwnNumber(a.modem.Status())
	values, _ := a.store.AllSettings()
	if values == nil {
		values = map[string]string{}
	}
	return api.SettingsPayload{
		OwnNumber:       own,
		OwnNumberSource: src,
		Values:          values,
		APITokenSet:     a.cfg.APIToken != "",
	}
}

// UpdateSettings 更新设置项。目前只支持 own_number。
func (a *App) UpdateSettings(kv map[string]string) error {
	for k, v := range kv {
		switch k {
		case "own_number":
			v = strings.TrimSpace(v)
			if v == "" {
				// 清空 = 回到「跟随 SIM / 配置文件」
				if err := a.store.DeleteSetting("own_number"); err != nil {
					return err
				}
			} else if err := a.store.SetSetting("own_number", v); err != nil {
				return err
			}
		default:
			return fmt.Errorf("不支持的设置项: %s", k)
		}
	}
	return nil
}

// ListMessages 分页查询归档。
func (a *App) ListMessages(q store.ListQuery) ([]store.Message, int, error) {
	return a.store.List(q)
}

// ForwardTest 发一条测试消息到钉钉。
func (a *App) ForwardTest() error {
	if a.relay == nil {
		return fmt.Errorf("钉钉转发未启用")
	}
	return a.relay.Send(a.cfg.DingTalk.Prefix + "短信网关联通性测试")
}

// ResetForward 让某条记录重新进入转发队列。
func (a *App) ResetForward(id int64) error {
	return a.store.ResetForward(id)
}

// DeleteMessage 删除一条归档记录。
func (a *App) DeleteMessage(id int64) error {
	return a.store.Delete(id)
}

// Token 返回 API 访问口令，空表示不鉴权。
func (a *App) Token() string { return a.cfg.APIToken }
