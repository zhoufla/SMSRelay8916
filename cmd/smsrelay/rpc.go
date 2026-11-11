package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zhoufla/SMSRelay8916/internal/config"
	"github.com/zhoufla/SMSRelay8916/internal/localrpc"
	"github.com/zhoufla/SMSRelay8916/internal/logbuf"
	"github.com/zhoufla/SMSRelay8916/internal/modem"
	"github.com/zhoufla/SMSRelay8916/internal/store"
)

// rpcd exec 插件协议：list 描述白名单方法，call 从 stdin 读取参数。
var rpcMethods = map[string]map[string]string{
	"status": {}, "messages": {"dir": "String", "q": "String", "has_code": "Boolean", "limit": "Integer", "offset": "Integer"},
	"message": {"id": "Integer"}, "delete": {"ids": "Array"},
	"send":      {"request_id": "String", "number": "String", "text": "String"},
	"send_task": {"id": "Integer"}, "forward": {"id": "Integer"},
	"logs": {"limit": "Integer", "level": "String"}, "cleanup": {},
	"channels": {}, "test_channel": {"section": "String"},
	"ddns": {}, "sync_ddns": {"section": "String"},
	"operation": {"operation": "String"},
}

func runRPC(args []string) {
	enc := json.NewEncoder(os.Stdout)
	if len(args) == 1 && args[0] == "list" {
		_ = enc.Encode(rpcMethods)
		return
	}
	if len(args) != 2 || args[0] != "call" {
		_ = enc.Encode(localrpc.Response{Error: "非法 RPC 调用"})
		return
	}
	if _, ok := rpcMethods[args[1]]; !ok {
		_ = enc.Encode(localrpc.Response{Error: "不支持的方法"})
		return
	}
	params, err := io.ReadAll(io.LimitReader(os.Stdin, localrpc.MaxRequest+1))
	if err != nil || len(params) > localrpc.MaxRequest || !json.Valid(params) {
		_ = enc.Encode(localrpc.Response{Error: "非法参数"})
		return
	}
	out, err := localrpc.Call(localrpc.Socket, localrpc.Request{Method: args[1], Params: params})
	if err != nil {
		_ = enc.Encode(localrpc.Response{Error: err.Error()})
		return
	}
	_, _ = os.Stdout.Write(out)
}

func (a *App) cachedModemStatus() *modem.Status {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	// 查询在后台执行，慢 mmcli 不阻塞 LuCI/rpcd。
	if a.modemStatus == nil {
		a.modemStatus = &modem.Status{State: "正在读取"}
	}
	if time.Since(a.statusAt) > 30*time.Second {
		a.statusAt = time.Now()
		go func() { st := a.modem.Status(); a.statusMu.Lock(); a.modemStatus = st; a.statusMu.Unlock() }()
	}
	copy := *a.modemStatus
	return &copy
}

var numberPattern = regexp.MustCompile(`^\+?[0-9]{5,20}$`)
var requestPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)

func (a *App) dispatch(method string, params json.RawMessage) (interface{}, error) {
	if _, ok := rpcMethods[method]; !ok {
		return nil, fmt.Errorf("不支持的方法")
	}
	var p struct {
		ID        int64   `json:"id"`
		IDs       []int64 `json:"ids"`
		Dir       string  `json:"dir"`
		Q         string  `json:"q"`
		HasCode   bool    `json:"has_code"`
		Limit     int     `json:"limit"`
		Offset    int     `json:"offset"`
		Level     string  `json:"level"`
		RequestID string  `json:"request_id"`
		Number    string  `json:"number"`
		Text      string  `json:"text"`
		Section   string  `json:"section"`
		Operation string  `json:"operation"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误")
	}
	switch method {
	case "status":
		st := a.Status()
		if _, err := a.store.Counts(); err != nil {
			st.DatabaseError = "归档库不可用，请检查存储空间及日志"
		}
		return st, nil
	case "messages":
		if p.Dir != "in" && p.Dir != "out" {
			return nil, fmt.Errorf("请选择收件箱或发件箱")
		}
		if p.Limit < 1 {
			p.Limit = 30
		}
		if p.Limit > 100 {
			p.Limit = 100
		}
		if p.Offset < 0 {
			p.Offset = 0
		}
		if len(p.Q) > 512 {
			return nil, fmt.Errorf("搜索词过长")
		}
		items, total, err := a.store.List(store.ListQuery{Dir: p.Dir, Q: p.Q, HasCode: p.HasCode, Limit: p.Limit, Offset: p.Offset})
		if items == nil {
			items = []store.Message{}
		}
		return map[string]interface{}{"items": items, "total": total}, err
	case "message":
		if p.ID <= 0 {
			return nil, fmt.Errorf("非法记录编号")
		}
		return a.store.Get(p.ID)
	case "delete":
		if len(p.IDs) < 1 || len(p.IDs) > 100 {
			return nil, fmt.Errorf("一次请选择 1–100 条记录")
		}
		for _, id := range p.IDs {
			if id <= 0 {
				return nil, fmt.Errorf("非法记录编号")
			}
		}
		n, err := a.store.DeleteByIDs(p.IDs)
		return map[string]int64{"deleted": n}, err
	case "send":
		p.Number = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(p.Number))
		p.Text = strings.TrimSpace(p.Text)
		if !numberPattern.MatchString(p.Number) || p.Text == "" || len([]rune(p.Text)) > 1000 || strings.ContainsRune(p.Text, 0) {
			return nil, fmt.Errorf("号码不合法或正文为空/超过 1000 字")
		}
		if !requestPattern.MatchString(p.RequestID) {
			return nil, fmt.Errorf("缺少有效请求标识")
		}
		id, err := a.store.QueueSend(p.RequestID, p.Number, p.Text, splitBody(p.Text))
		return map[string]int64{"id": id}, err
	case "send_task":
		return a.store.SendTask(p.ID)
	case "forward":
		m, err := a.store.Get(p.ID)
		if err != nil {
			return nil, err
		}
		if m.Dir != "in" {
			return nil, fmt.Errorf("只有收件可以转发")
		}
		return nil, a.store.ResetForward(p.ID)
	case "logs":
		if p.Limit < 1 || p.Limit > 500 {
			p.Limit = 500
		}
		items := a.logs.Snapshot(p.Limit, p.Level)
		if items == nil {
			items = []logbuf.Entry{}
		}
		return map[string]interface{}{"items": items}, nil
	case "cleanup":
		n, err := a.CleanupNow()
		return map[string]int64{"deleted": n}, err
	case "channels":
		// 以 UCI 段名作为浏览器可往返的标识，避免 JS 丢失 int64 精度。
		items := []map[string]interface{}{}
		for _, ch := range a.cfg.Channels {
			items = append(items, map[string]interface{}{"name": ch.Name, "enabled": ch.Enabled})
		}
		return map[string]interface{}{"items": items}, nil
	case "test_channel":
		if a.cfg.ChannelByID(config.SectionID(p.Section)) == nil {
			return nil, fmt.Errorf("通道不存在，请先保存并应用配置")
		}
		return a.startOperation(func() (string, error) { return "测试成功", a.TestChannel(config.SectionID(p.Section)) })
	case "ddns":
		items, err := a.ListDDNS()
		if err != nil {
			return nil, err
		}
		// 不返回凭据；ID 使用字符串，浏览器不进行数值转换。
		out := []map[string]interface{}{}
		for _, r := range items {
			out = append(out, map[string]interface{}{"name": r.Name, "type": r.Type, "last_ip": r.LastIP, "last_update": r.LastUpdate, "last_error": r.LastError, "reach_note": r.ReachNote})
		}
		return map[string]interface{}{"items": out}, nil
	case "sync_ddns":
		if a.cfg.DDNSByID(config.SectionID(p.Section)) == nil {
			return nil, fmt.Errorf("记录不存在，请先保存并应用配置")
		}
		return a.startOperation(func() (string, error) { return a.SyncDDNSNow(config.SectionID(p.Section)) })
	case "operation":
		a.operationMu.Lock()
		defer a.operationMu.Unlock()
		op, ok := a.operations[p.Operation]
		if !ok {
			return nil, fmt.Errorf("操作不存在或后台已重启")
		}
		copy := *op
		return copy, nil
	}
	return nil, fmt.Errorf("不支持的方法")
}

type operation struct {
	Done    bool   `json:"done"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

func (a *App) startOperation(fn func() (string, error)) (interface{}, error) {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.operationBusy {
		return nil, fmt.Errorf("已有测试/同步操作执行中，请稍后重试")
	}
	if a.operations == nil {
		a.operations = make(map[string]*operation)
	}
	a.operationSeq++
	id := fmt.Sprintf("op-%d", a.operationSeq)
	delete(a.operations, fmt.Sprintf("op-%d", a.operationSeq-32))
	a.operations[id] = &operation{}
	a.operationBusy = true
	go func() {
		msg, err := fn()
		a.operationMu.Lock()
		defer a.operationMu.Unlock()
		op := a.operations[id]
		op.Done = true
		op.Message = msg
		if err != nil {
			log.Printf("WARN 操作 %s 失败: %v", id, err)
			op.Error = "操作失败，详见日志"
		}
		a.operationBusy = false
	}()
	return map[string]string{"operation": id}, nil
}
