// Package eventbus 是一个极简的进程内发布订阅中心。
//
// 它的职责只有一个：把业务事件（新短信、系统指标、modem 状态变化）广播给
// SSE 订阅者。刻意不做持久化、不做重放、不做阻塞投递 —— 收短信是主业务，
// 一个慢客户端绝不能把它拖住。
package eventbus

import (
	"sync"
	"time"
)

// Event 是推送给客户端的一条事件。
type Event struct {
	Type string      `json:"-"`
	Data interface{} `json:"data"`
	TS   string      `json:"ts"`
}

// subBuffer 是每个订阅者的缓冲深度。页面一次最多消费几十条，
// 16 足够吸收突发；溢出时丢弃，客户端靠重新拉取兜底。
const subBuffer = 16

// Bus 是订阅中心。
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
}

// New 创建总线。
func New() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe 注册一个订阅者，返回 id 与只读 channel。
// 订阅者退出时必须调用 Unsubscribe，否则 channel 会一直被广播。
func (b *Bus) Subscribe() (int, <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	id := b.nextID
	ch := make(chan Event, subBuffer)
	b.subs[id] = ch
	return id, ch
}

// Unsubscribe 移除订阅者并关闭其 channel。
func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Subscribers 返回当前订阅者数量（用于状态展示与限流）。
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Publish 广播事件。
//
// 某个订阅者的 channel 满时直接丢弃这条事件，绝不阻塞调用方 ——
// 宁可让一个页面少一条更新，也不能让短信归档流程被卡住。
func (b *Bus) Publish(typ string, data interface{}) {
	ev := Event{
		Type: typ,
		Data: data,
		TS:   time.Now().Format(time.RFC3339),
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
