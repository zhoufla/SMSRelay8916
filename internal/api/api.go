// Package api 提供 HTTP 接口与内置 Web UI。
//
// 接口与早期 Python 版完全兼容：现有前端页面不用改一行，
// 连返回的 JSON 字段名都保持一致。
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhoufla/SMSRelay8916/internal/eventbus"
	"github.com/zhoufla/SMSRelay8916/internal/forward"
	"github.com/zhoufla/SMSRelay8916/internal/store"
)

//go:embed web/index.html
var embeddedIndex []byte

// SendPart 是单条（拆分后的）发送结果。
type SendPart struct {
	OK        bool   `json:"ok"`
	State     string `json:"state,omitempty"`
	Reference string `json:"message_reference,omitempty"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
}

// SendOutcome 是一次发送请求的整体结果。
type SendOutcome struct {
	OK      bool       `json:"ok"`
	Parts   int        `json:"parts"`
	Results []SendPart `json:"results,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// GatewayInfo 是网关自身的信息。
type GatewayInfo struct {
	Uptime   int    `json:"uptime"`
	Hostname string `json:"hostname"`
	DingTalk bool   `json:"dingtalk"`
	Version  string `json:"version"`
}

// StatusPayload 是 /api/status 的返回体。
type StatusPayload struct {
	Modem           interface{}  `json:"modem"`
	OwnNumber       string       `json:"own_number"`
	OwnNumberSource string       `json:"own_number_source"`
	Counts          store.Counts `json:"counts"`
	Today           store.Today  `json:"today"`
	LastInbound     string       `json:"last_inbound"`
	Paging          store.Paging `json:"paging_24h"`
	Gateway         GatewayInfo  `json:"gateway"`
}

// SettingsPayload 是 /api/settings 的返回体。
type SettingsPayload struct {
	OwnNumber       string            `json:"own_number"`
	OwnNumberSource string            `json:"own_number_source"`
	Values          map[string]string `json:"values"`
	APITokenSet     bool              `json:"api_token_set"`
}

// Backend 由 main 实现，提供业务能力。用接口是为了避免 api 反向依赖 main。
type Backend interface {
	Status() StatusPayload
	System() interface{}
	Settings() interface{}
	UpdateSettings(map[string]string) error
	ListMessages(q store.ListQuery) ([]store.Message, int, error)
	SendSMS(number, text string) SendOutcome
	ForwardTest() error
	ResetForward(id int64) error
	DeleteMessage(id int64) error
	ListChannels() ([]forward.Channel, error)
	CreateChannel(*forward.Channel) error
	UpdateChannel(*forward.Channel) error
	DeleteChannel(int64) error
	TestChannel(int64) error
	Token() string
}

// Server 是 HTTP 服务。
type Server struct {
	backend Backend
	bus     *eventbus.Bus
	webDir  string // 若存在外部 web 目录则优先使用，便于改页面不用重编译

	mu      sync.Mutex
	sseSubs int // 当前 SSE 连接数，防止被误用拖出几百个 goroutine
}

// maxSSE 是同时允许的 SSE 连接上限。
const maxSSE = 8

// New 创建 HTTP 服务。webDir 可为空。
func New(b Backend, bus *eventbus.Bus, webDir string) *Server {
	return &Server{backend: b, bus: bus, webDir: webDir}
}

var idPattern = regexp.MustCompile(`^/api/messages/(\d+)$`)

var (
	channelIDRe   = regexp.MustCompile(`^/api/channels/(\d+)$`)
	channelTestRe = regexp.MustCompile(`^/api/channels/(\d+)/test$`)
)

func pathID(re *regexp.Regexp, path string) int64 {
	m := re.FindStringSubmatch(path)
	if len(m) < 2 {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// Handler 返回已装配路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	return mux
}

// Listen 启动 HTTP 服务（阻塞）。
func (s *Server) Listen(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("HTTP 监听 %s", addr)
	return srv.ListenAndServe()
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 首页与静态页不需要鉴权（页面本身不含数据）
	if path == "/" || path == "/index.html" {
		s.serveIndex(w)
		return
	}
	if path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/api/ping":
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case r.Method == http.MethodGet && path == "/api/status":
		writeJSON(w, http.StatusOK, s.backend.Status())

	case r.Method == http.MethodGet && path == "/api/system":
		v := s.backend.System()
		if v == nil {
			// 首次采样还没完成，返回空对象而不是 null，前端不用特判
			v = map[string]string{}
		}
		writeJSON(w, http.StatusOK, v)

	case r.Method == http.MethodGet && path == "/api/settings":
		writeJSON(w, http.StatusOK, s.backend.Settings())

	case r.Method == http.MethodPut && path == "/api/settings":
		s.updateSettings(w, r)

	case path == "/api/channels" && r.Method == http.MethodGet:
		s.listChannels(w)
	case path == "/api/channels" && r.Method == http.MethodPost:
		s.createChannel(w, r)
	case channelTestRe.MatchString(path) && r.Method == http.MethodPost:
		s.testChannel(w, r, pathID(channelTestRe, path))
	case channelIDRe.MatchString(path):
		s.channelByID(w, r, pathID(channelIDRe, path))

	case r.Method == http.MethodGet && path == "/api/events":
		s.events(w, r)

	case r.Method == http.MethodGet && path == "/api/messages":
		s.listMessages(w, r)

	case r.Method == http.MethodPost && path == "/api/send":
		s.send(w, r)

	case r.Method == http.MethodPost && path == "/api/forward-test":
		if err := s.backend.ForwardTest(); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"ok": false, "error": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case r.Method == http.MethodPost && path == "/api/forward":
		var body struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		if err := s.backend.ResetForward(body.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case r.Method == http.MethodDelete:
		m := idPattern.FindStringSubmatch(path)
		if m == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		if err := s.backend.DeleteMessage(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (s *Server) authorized(r *http.Request) bool {
	token := s.backend.Token()
	if token == "" {
		return true
	}
	if r.Header.Get("X-Token") == token {
		return true
	}
	return r.URL.Query().Get("token") == token
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	// 优先外部文件，方便改页面后刷新即可生效
	if s.webDir != "" {
		p := filepath.Join(s.webDir, "index.html")
		if b, err := os.ReadFile(p); err == nil {
			writeHTML(w, b)
			return
		}
	}
	writeHTML(w, embeddedIndex)
}

// events 是 SSE 端点。
//
// 选 SSE 而不是 WebSocket：页面只需要服务器单向推送，而 EventSource 是浏览器
// 原生支持且自带重连，不需要握手库、不需要额外依赖，也不需要维护连接状态机。
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "当前服务器不支持流式响应"})
		return
	}

	s.mu.Lock()
	if s.sseSubs >= maxSSE {
		s.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": fmt.Sprintf("SSE 连接数已达上限 %d", maxSSE)})
		return
	}
	s.sseSubs++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sseSubs--
		s.mu.Unlock()
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// 关掉反向代理缓冲，否则事件会被攒着一起发
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	id, ch := s.bus.Subscribe()
	defer s.bus.Unsubscribe(id)

	// 首帧带完整状态，客户端不用再单独发几个请求
	if hello, err := json.Marshal(map[string]interface{}{
		"status": s.backend.Status(),
		"system": s.backend.System(),
	}); err == nil {
		fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello)
		flusher.Flush()
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// 注释行心跳：客户端忽略内容，但能防止中间设备按空闲超时断开
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var kv map[string]string
	if err := json.NewDecoder(r.Body).Decode(&kv); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	if err := s.backend.UpdateSettings(kv); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 回写完整设置，前端不用再发一次 GET
	writeJSON(w, http.StatusOK, s.backend.Settings())
}

func (s *Server) listChannels(w http.ResponseWriter) {
	chs, err := s.backend.ListChannels()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if chs == nil {
		chs = []forward.Channel{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": chs})
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var ch forward.Channel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	if err := s.backend.CreateChannel(&ch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": ch.ID})
}

func (s *Server) channelByID(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodPut:
		var ch forward.Channel
		if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		ch.ID = id
		if err := s.backend.UpdateChannel(&ch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := s.backend.DeleteChannel(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.backend.TestChannel(id); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 30
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}

	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	if offset < 0 {
		offset = 0
	}

	lq := store.ListQuery{
		Dir:    q.Get("dir"),
		Q:      strings.TrimSpace(q.Get("q")),
		Limit:  limit,
		Offset: offset,
	}
	items, total, err := s.backend.ListMessages(lq)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if items == nil {
		items = []store.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"items":  items,
	})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number string `json:"number"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}

	number := normalizeNumber(body.Number)
	text := strings.TrimSpace(body.Text)

	if number == "" || text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "number 与 text 必填"})
		return
	}
	if !isValidNumber(number) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "号码格式不合法"})
		return
	}

	out := s.backend.SendSMS(number, text)
	// 与旧版一致：业务失败也返回 200，由 ok 字段表达
	writeJSON(w, http.StatusOK, out)
}

var numberPattern = regexp.MustCompile(`^\+?\d{5,20}$`)

func normalizeNumber(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '-', '(', ')':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isValidNumber(s string) bool {
	return numberPattern.MatchString(s)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"序列化失败"}`), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(b)
}

func writeHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}
