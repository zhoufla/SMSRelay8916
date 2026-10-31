package modem

import (
	"context"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	// ModemManager 把短信增删事件发在 Modem 对象的 Messaging 接口上：
	//   Added(o path, b received)
	//   Deleted(o path)
	messagingIface = "org.freedesktop.ModemManager1.Modem.Messaging"
	addedSignal    = messagingIface + ".Added"
	deletedSignal  = messagingIface + ".Deleted"
)

// WatchEvent 是一次短信对象变化通知。
type WatchEvent struct {
	Path    string
	Deleted bool
}

// Watch 订阅 ModemManager 的短信增删信号，直到 ctx 结束。
//
// 这是替代"每 3 秒轮询一次"的关键：有短信才动，空闲时完全不产生子进程。
// 如果系统总线不可用（例如在容器里跑），调用方应降级为纯轮询。
func (c *Client) Watch(ctx context.Context, onEvent func(WatchEvent)) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("连接系统总线失败: %w", err)
	}

	// 只订阅 Messaging 接口的信号；不限定 object path，
	// 这样 modem 编号变化（Modem/0 -> Modem/1）也能收到。
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(messagingIface),
	); err != nil {
		return fmt.Errorf("订阅 D-Bus 信号失败: %w", err)
	}

	ch := make(chan *dbus.Signal, 128)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case sig, ok := <-ch:
			if !ok {
				return nil
			}
			if sig == nil {
				continue
			}
			if sig.Name != addedSignal && sig.Name != deletedSignal {
				continue
			}
			if len(sig.Body) == 0 {
				continue
			}
			path, ok := sig.Body[0].(dbus.ObjectPath)
			if !ok {
				continue
			}
			p := string(path)
			if !strings.HasPrefix(p, smsPathPrefix) {
				continue
			}
			onEvent(WatchEvent{Path: p, Deleted: sig.Name == deletedSignal})
		}
	}
}
