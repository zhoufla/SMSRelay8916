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
//   - 界面：原生 LuCI 经 rpcd 与本地 Unix Socket 通信，不监听 TCP。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/zhoufla/SMSRelay8916/internal/api"
	"github.com/zhoufla/SMSRelay8916/internal/config"
	"github.com/zhoufla/SMSRelay8916/internal/ddns"
	"github.com/zhoufla/SMSRelay8916/internal/forward"
	"github.com/zhoufla/SMSRelay8916/internal/localrpc"
	"github.com/zhoufla/SMSRelay8916/internal/logbuf"
	"github.com/zhoufla/SMSRelay8916/internal/modem"
	"github.com/zhoufla/SMSRelay8916/internal/otp"
	"github.com/zhoufla/SMSRelay8916/internal/store"
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
	// receivedHold：MM 已回报终态（received/sent/delivered/failed）时正文必然收齐，
	// 只留 1 秒防抖即归档；保守 holdSeconds 只用于中间态兜底。
	// 这是收信延时的主要开关：单条短信从 D-Bus 推送到归档可压进 2~3 秒。
	receivedHold = 1 * time.Second
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

// Config 由 internal/config 从 UCI 解析，这里不再保留第二份定义。

// smsState 记录一个 SMS 对象在本进程内的观察历史。
type smsState struct {
	firstSeen  time.Time // 首次看到该对象的时间，用于 pending 超时兜底
	lastChange time.Time // 正文/状态最后一次变化的时间，用于稳定性判断
	lastSig    string
	failures   int // 连续读取失败次数，用于指数退避与放弃判定
}

// App 装配短信业务、本地 RPC 和持久发送队列。
//
// 配置（含通道、DDNS）全部来自 UCI，进程内只读：改配置走 LuCI，
// uci commit 触发 procd reload，服务重启后读到新值。程序不写配置，
// 所以不存在"两个写入口互相覆盖"的问题。
type App struct {
	cfg      *config.Config
	store    *store.Store
	modem    *modem.Client
	sender   *forward.Sender
	cf       *ddns.Client
	logs     *logbuf.Buffer // 运行日志环形缓冲，供 /api/logs 查看
	startTS  time.Time
	hostname string

	events chan string // 待处理的 SMS 对象路径

	mu     sync.Mutex // 保护 states
	states map[string]*smsState

	activeMu      sync.Mutex
	activePaths   map[string]bool
	statusMu      sync.Mutex
	modemStatus   *modem.Status
	statusAt      time.Time
	operationMu   sync.Mutex
	operations    map[string]*operation
	operationSeq  int
	operationBusy bool
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "backup" || os.Args[1] == "restore") {
		databaseCommand(os.Args[1])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "rpc" {
		runRPC(os.Args[2:])
		return
	}
	var (
		configFlag = flag.String("config", config.DefaultPath, "UCI 配置路径")
		checkFlag  = flag.Bool("check", false, "仅校验配置与依赖，不启动服务")
		printFlag  = flag.Bool("print-config", false, "打印生效的配置后退出")
		versionFlg = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags)
	// stderr 进入 OpenWrt logd；内存环形缓冲供 LuCI 日志页查看。
	ring := logbuf.New(500)
	log.SetOutput(io.MultiWriter(os.Stderr, ring))

	if *versionFlg {
		fmt.Printf("smsrelay %s\n", Version)
		return
	}

	cfg, err := config.Load(*configFlag)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	secrets := []string{}
	for _, ch := range cfg.Channels {
		secrets = append(secrets, ch.Config.Secret, ch.Config.URL)
		for _, v := range ch.Config.Headers {
			if len(v) > 8 {
				secrets = append(secrets, v)
			}
		}
	}
	for _, r := range cfg.DDNS {
		secrets = append(secrets, r.APIToken)
	}
	log.SetOutput(logbuf.RedactingWriter(io.MultiWriter(os.Stderr, ring), secrets))
	log.Printf("配置: %s（通道 %d 个、DDNS %d 条）", cfg.Source, len(cfg.Channels), len(cfg.DDNS))
	for _, w := range cfg.Warnings {
		log.Printf("WARN %s", w)
	}

	if *printFlag {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return
	}
	if !cfg.Enabled {
		log.Printf("UCI 里 enabled=0，服务不启动")
		return
	}
	// 先占用单实例 Socket，再恢复发送队列，避免第二个进程改动活跃任务。
	var listener net.Listener
	if !*checkFlag {
		listener, err = localrpc.Listen(localrpc.Socket)
		if err != nil {
			log.Fatalf("本地 RPC 启动失败: %v", err)
		}
		defer listener.Close()
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("ERROR 归档库无法打开，保留数据库与模组短信，请检查存储: %v", err)
	}
	defer st.Close()
	log.Printf("归档库: %s", cfg.DBPath)

	hostname, _ := os.Hostname()
	app := &App{
		cfg:         cfg,
		store:       st,
		modem:       modem.New(cfg.ModemIndex),
		logs:        ring,
		startTS:     time.Now(),
		hostname:    hostname,
		events:      make(chan string, 512),
		states:      make(map[string]*smsState),
		activePaths: make(map[string]bool),
	}
	app.sender = forward.NewSender()
	app.cf = ddns.NewClient()

	// 通道从 UCI 删掉后，库里按 channel_id 记的投递行就成了孤儿：
	// 功能上无害，但会让"是否已全部转发"永远为假，启动时清一次。
	if n, perr := st.PruneDeliveries(channelIDs(cfg.Channels)); perr == nil && n > 0 {
		log.Printf("清理已删除通道的投递记录: %d 条", n)
	}

	if chs := cfg.EnabledChannels(); len(chs) > 0 {
		names := make([]string, 0, len(chs))
		for _, ch := range chs {
			names = append(names, ch.Name)
		}
		log.Printf("转发通道: %d 个已启用（%s）", len(chs), strings.Join(names, "、"))
	} else {
		log.Printf("转发通道: 未配置")
	}

	if *checkFlag {
		st0 := app.modem.Status()
		fmt.Printf("配置 OK\n  db      : %s\n  modem   : %s / %s / %s\n  sim     : %s\n",
			cfg.DBPath, st0.State, st0.AccessTech, st0.Registration, st0.IMSISim)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := st.RecoverSendTasks(); err != nil {
		log.Fatalf("恢复发送队列失败: %v", err)
	}

	// 1) 事件处理 worker（串行，避免同一对象被并发读写）
	go app.worker(ctx)

	// 2) D-Bus 信号订阅；失败则退化为纯轮询
	go app.watchLoop(ctx)

	// 3) 兜底轮询：防止信号丢失导致短信永远躺在 modem 里
	go app.guardLoop(ctx)

	// 4) 转发队列
	go app.forwardLoop(ctx)

	// 5) DDNS：启动 15 秒后先跑一次，之后每 5 分钟一次
	go app.ddnsLoop(ctx, 5*time.Minute)

	// 6) 归档保留：按 retain_days 设置周期清理过期记录（设置里改为 0 即关闭）
	go app.retentionLoop(ctx, 6*time.Hour)
	go app.sendLoop(ctx)

	go func() {
		log.Printf("本地 RPC 已就绪，不监听 TCP 端口")
		if err := localrpc.Serve(ctx, listener, app.dispatch); err != nil {
			log.Printf("ERROR RPC 服务异常退出: %v", err)
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
	a.activeMu.Lock()
	active := a.activePaths[path]
	a.activeMu.Unlock()
	if active {
		a.requeue(path, requeueDelay)
		return
	}
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
	origState := state
	// pendingMax 超时后的兜底状态：已经等了 30 秒，归档不再追加保守 hold。
	forcedState := false
	if sms.PDUType == "submit" && state != "sent" && state != "delivered" && state != "failed" {
		return
	}
	if pendingStates[state] {
		// 出站中间态不能靠超时猜成 sent，尤其是重启后待确认的发送任务。
		if sms.PDUType == "submit" {
			return
		}
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
		forcedState = true
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
	// 自适应稳定期：MM 报终态（或 pendingMax 兜底）时正文已收齐，走短防抖；
	// 其余（分片重组中、状态未知）维持保守 holdSeconds 防长短信分片撞车。
	hold := holdSeconds
	switch origState {
	case "received", "sent", "delivered", "failed":
		hold = receivedHold
	}
	if forcedState {
		hold = receivedHold
	}
	if now.Sub(lastChange) < hold {
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
		// 验证码是正文的派生数据，归档入口也会做幂等兜底。
		msg.Code = otp.Extract(body)
	}

	archived, err := a.store.Archive(msg)
	if err != nil {
		// 入库失败绝不删除，宁可让它留在 modem 里下一轮再试
		log.Printf("WARN 归档失败，保留在原处 %s: %v", path, err)
		a.requeue(path, 5*time.Second)
		return
	}
	if !archived {
		log.Printf("WARN 未确认归档，保留在原处 %s", path)
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
		log.Printf("in 短信已归档 (%s)", ts)
	}
}

// ---------------------------------------------------------------------------
// 发送
// ---------------------------------------------------------------------------

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
	gsm := isGSM7(body)
	if !gsm {
		limit = 70
	}
	runes := []rune(body)
	units := func(r rune) int {
		if !gsm && utf16.RuneLen(r) == 2 {
			return 2
		}
		return 1
	}
	size := 0
	for _, r := range runes {
		size += units(r)
	}
	if size <= limit {
		return []string{body}
	}
	total := 2
	for {
		out := []string{}
		for idx := 0; idx < len(runes); {
			marker := fmt.Sprintf(" (%d/%d)", len(out)+1, total)
			end, used := idx, len(marker)
			for end < len(runes) && used+units(runes[end]) <= limit {
				used += units(runes[end])
				end++
			}
			out = append(out, string(runes[idx:end])+marker)
			idx = end
		}
		if len(out) == total {
			return out
		}
		total = len(out)
	}
}

// ---------------------------------------------------------------------------
// 转发
// ---------------------------------------------------------------------------

// forwardLoop 是多通道转发循环。
func (a *App) forwardLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.forwardBatch(ctx)
		}
	}
}

// forwardBatch 取一批待转发短信，对每条短信遍历所有启用通道。
//
// 每个通道的成败单独记录在 channel_deliveries 里：重试时已经成功的通道会被跳过，
// 不会因为某个通道挂了就把成功的那个反复重推。
func (a *App) forwardBatch(ctx context.Context) {
	msgs, err := a.store.PendingForward(5)
	if err != nil || len(msgs) == 0 {
		return
	}
	channels := a.cfg.EnabledChannels()
	if len(channels) == 0 {
		return
	}
	own, _ := a.resolveOwnNumber(nil)

	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		allOK := true
		for _, ch := range channels {
			if a.store.DeliveryStatus(m.ID, ch.ID) == "sent" {
				continue
			}
			v := forward.Vars{
				OwnNumber: own,
				Number:    m.Peer,
				Body:      m.Body,
				Code:      m.Code,
				TS:        fmtForwardTime(m.TS),
				ID:        m.ID,
				Direction: m.Dir,
			}
			// 条件不满足的通道直接跳过：不投递、不记投递记录、不算失败。
			// 若所有通道都不满足，allOK 保持 true → 消息标记 forwarded（无事可做）。
			if !ch.Matches(v) {
				continue
			}
			if err := a.sender.Send(ch, v); err != nil {
				_ = a.store.MarkDelivery(m.ID, ch.ID, "failed", err.Error())
				log.Printf("WARN forward #%d -> %s 失败: %v", m.ID, ch.Name, err)
				allOK = false
				continue
			}
			_ = a.store.MarkDelivery(m.ID, ch.ID, "sent", "")
			log.Printf("forwarded #%d -> %s", m.ID, ch.Name)
		}
		if allOK {
			_ = a.store.MarkForwarded(m.ID)
		} else {
			_ = a.store.MarkForwardFailed(m.ID, "部分通道投递失败")
		}
	}
}

// fmtForwardTime 把 2026-10-31T21:42:40+08 变成 2026-10-31 21:42:40。
func fmtForwardTime(ts string) string {
	if strings.Contains(ts, "T") {
		ts = strings.Replace(ts, "T", " ", 1)
		if len(ts) > 19 {
			ts = ts[:19]
		}
	}
	return ts
}

// ---------------------------------------------------------------------------
// LuCI 数据与维护操作
// ---------------------------------------------------------------------------

// Status 汇总 modem、归档库与网关自身状态。
func (a *App) Status() api.StatusPayload {
	st := a.cachedModemStatus()
	counts, _ := a.store.Counts()
	today, _ := a.store.Today()
	pg, _ := a.store.Paging24h()
	own, src := a.resolveOwnNumber(st)
	enabledChannels := a.cfg.EnabledChannels()

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
			DingTalk: len(enabledChannels) > 0,
			Version:  Version,
		},
	}
}

// resolveOwnNumber 解析本机号码，返回号码与来源。
//
// 优先级：UCI own_number > SIM 卡 > 空。
// SIM 里读不到号码是常态（很多卡不写 EF_MSISDN，本机这张就没有），
// 所以必须允许手动兜底，否则转发内容里永远缺"收信号码"。
func (a *App) resolveOwnNumber(st *modem.Status) (string, string) {
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

// ListMessages 分页查询归档。
func (a *App) ListMessages(q store.ListQuery) ([]store.Message, int, error) {
	return a.store.List(q)
}

// ForwardTest 向所有启用的通道各发一条测试消息。
// 返回第一条失败的错误，方便定位是哪个通道配错了。
func (a *App) ForwardTest() error {
	chs := a.cfg.EnabledChannels()
	if len(chs) == 0 {
		return fmt.Errorf("没有启用的转发通道")
	}
	own, _ := a.resolveOwnNumber(nil)
	v := forward.Vars{
		OwnNumber: own,
		Number:    "10086",
		Body:      "短信网关联通性测试",
		TS:        time.Now().Format("2006-01-02 15:04:05"),
		Direction: "in",
	}
	var firstErr error
	for _, ch := range chs {
		if err := a.sender.Send(ch, v); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", ch.Name, err)
		}
	}
	return firstErr
}

// ResetForward 让某条记录重新进入转发队列。
func (a *App) ResetForward(id int64) error {
	return a.store.ResetForward(id)
}

// DeleteMessage 删除一条归档记录。
func (a *App) DeleteMessage(id int64) error {
	return a.store.Delete(id)
}

// DeleteMessages 批量删除归档记录（API 层已做数量与空校验）。
func (a *App) DeleteMessages(ids []int64) (int64, error) {
	return a.store.DeleteByIDs(ids)
}

// CleanupNow 按当前 retain_days 立即清一次过期记录。
// 为 0 时不动作，返回 0 —— 与「立即清理」按钮的语义一致。
func (a *App) CleanupNow() (int64, error) {
	days := a.cfg.RetainDays
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	deleted, err := a.store.DeleteOlderThan(cutoff)
	if err == nil && deleted > 0 {
		log.Printf("手动清理: 删除 %d 条过期记录（保留 %d 天）", deleted, days)
	}
	return deleted, err
}

// Logs 返回内存环形缓冲里的运行日志。
func (a *App) Logs(limit int, level string) []logbuf.Entry {
	return a.logs.Snapshot(limit, level)
}

// retentionLoop 周期清理超过保留天数的归档记录。
//
// 放在 Go 进程内而不是 maint.sh：进程持有 DB 连接无需停服务，
// 设备上也不保证有 sqlite3 CLI；maint.sh 保持 modem 侧单一职责。
func (a *App) retentionLoop(ctx context.Context, iv time.Duration) {
	// 启动后先等一会儿，避开开机时归档/转发都还没跑起来的阶段
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Minute):
	}

	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			days := a.cfg.RetainDays
			if days <= 0 {
				continue
			}
			cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
			n, err := a.store.DeleteOlderThan(cutoff)
			if err != nil {
				log.Printf("WARN 周期清理失败: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("周期清理: 删除 %d 条过期记录（保留 %d 天，早于 %s）", n, days, cutoff)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 转发通道管理
// ---------------------------------------------------------------------------

// ListChannels 返回所有通道，敏感字段已脱敏。
//
// 通道来自 UCI，改配置在 LuCI 里做；这里只提供只读视图。
// 脱敏是必须的：URL 里带着 access_token，header 里可能带鉴权头。
func (a *App) ListChannels() ([]forward.Channel, error) {
	out := make([]forward.Channel, 0, len(a.cfg.Channels))
	for _, ch := range a.cfg.Channels {
		ch.Config = forward.MaskSecret(ch.Config)
		out = append(out, ch)
	}
	return out, nil
}

// TestChannel 只测指定的那个通道，便于逐个排查配置。
func (a *App) TestChannel(id int64) error {
	ch := a.cfg.ChannelByID(id)
	if ch == nil {
		return fmt.Errorf("通道不存在")
	}
	own, _ := a.resolveOwnNumber(nil)
	return a.sender.Send(*ch, forward.Vars{
		OwnNumber: own,
		Number:    "10086",
		Body:      "通道连通性测试",
		TS:        time.Now().Format("2006-01-02 15:04:05"),
		Direction: "in",
	})
}

// ---------------------------------------------------------------------------
// DDNS
// ---------------------------------------------------------------------------

// ddnsLoop 周期性把所有启用的 DDNS 记录同步到 Cloudflare。
func (a *App) ddnsLoop(ctx context.Context, iv time.Duration) {
	// 启动后先等一会儿，避开开机时网络还没就绪的阶段
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	a.ddnsSyncAll()

	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.ddnsSyncAll()
		}
	}
}

func (a *App) ddnsSyncAll() {
	for i := range a.cfg.DDNS {
		r := a.cfg.DDNS[i]
		if !r.Enabled {
			continue
		}
		a.syncDDNSRecord(&r)
	}
}

func (a *App) syncDDNSRecord(r *ddns.Record) {
	// 上次解析到的地址属于运行态，从库里取（配置在 UCI，没有这个字段）
	if st, err := a.store.GetDDNSState(r.ID); err == nil {
		r.LastIP = st.LastIP
	}
	changed, ip, err := a.cf.Sync(r, "")
	if err != nil {
		_ = a.store.SetDDNSState(r.ID, "", "同步失败，详见运行日志")
		log.Printf("WARN ddns %s (%s) 失败: %v", r.Name, r.Type, err)
		return
	}
	_ = a.store.SetDDNSState(r.ID, ip, "")
	if changed {
		log.Printf("ddns %s (%s) -> %s", r.Name, r.Type, ip)
	}
}

// ListDDNS 返回所有记录，附带实时地址与可达性判断。
func (a *App) ListDDNS() ([]ddns.Record, error) {
	recs := make([]ddns.Record, len(a.cfg.DDNS))
	copy(recs, a.cfg.DDNS)

	for i := range recs {
		if st, err := a.store.GetDDNSState(recs[i].ID); err == nil {
			recs[i].LastIP, recs[i].LastUpdate, recs[i].LastError = st.LastIP, st.LastUpdate, st.LastError
		}
		// API token 脱敏
		if t := recs[i].APIToken; t != "" {
			if len(t) <= 4 {
				recs[i].APIToken = "****"
			} else {
				recs[i].APIToken = "****" + t[len(t)-4:]
			}
		}

		// iface 来源读本地网卡，几乎零成本，直接实时算。
		// external 要走一次公网请求（还可能超时重试），绝不能放在列表接口里 ——
		// 之前这么干导致 /api/ddns 被拖到 30 秒以上，前端直接超时。
		if strings.HasPrefix(recs[i].Source, "iface:") {
			ip, rerr := ddns.ResolveIP(recs[i].Source, recs[i].Type, "")
			if rerr != nil {
				recs[i].Reachability = "unknown"
				recs[i].ReachNote = rerr.Error()
				continue
			}
			recs[i].CurrentIP = ip
			recs[i].Reachability, recs[i].ReachNote = ddns.Judge(recs[i], ip)
			continue
		}

		// external：只展示上次同步拿到的地址
		recs[i].CurrentIP = recs[i].LastIP
		if recs[i].LastIP != "" {
			recs[i].Reachability, recs[i].ReachNote = ddns.Judge(recs[i], recs[i].LastIP)
		} else {
			recs[i].Reachability = "unknown"
			recs[i].ReachNote = "尚未同步，点「立即同步」获取出口 IP"
		}
	}
	return recs, nil
}

// SyncDDNSNow 手动触发一次同步，便于配完立刻验证。
func (a *App) SyncDDNSNow(id int64) (string, error) {
	rec := a.cfg.DDNSByID(id)
	if rec == nil {
		return "", fmt.Errorf("记录不存在")
	}
	r := *rec
	if st, err := a.store.GetDDNSState(r.ID); err == nil {
		r.LastIP = st.LastIP
	}
	changed, ip, serr := a.cf.Sync(&r, "")
	if serr != nil {
		_ = a.store.SetDDNSState(r.ID, "", serr.Error())
		return "", serr
	}
	_ = a.store.SetDDNSState(r.ID, ip, "")
	if changed {
		return fmt.Sprintf("已更新为 %s", ip), nil
	}
	return fmt.Sprintf("无需更新（%s）", ip), nil
}

// channelIDs 取出全部通道主键，供清理孤儿投递记录用。
func channelIDs(chs []forward.Channel) []int64 {
	out := make([]int64, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.ID)
	}
	return out
}
