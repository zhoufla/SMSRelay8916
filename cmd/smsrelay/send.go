package main

import (
	"context"
	"log"
	"time"

	"github.com/zhoufla/SMSRelay8916/internal/store"
)

func (a *App) sendLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		task, err := a.store.ClaimSendTask()
		if err != nil {
			log.Printf("WARN 读取发送队列失败: %v", err)
			continue
		}
		if task == nil {
			continue
		}
		state, reason := "sent", ""
		for i, part := range task.Parts {
			if ctx.Err() != nil {
				return
			} // 下次启动标记 unknown，绝不自动重发。
			if state != "sent" {
				if err := a.store.UpdateSendPart(part.ID, "", "failed", "前一段未确认发送，后续分段未发送"); err != nil {
					log.Printf("ERROR 更新分段失败: %v", err)
				}
				continue
			}
			state, reason = a.sendQueuedPart(ctx, part)
			if state == "sent" && i < len(task.Parts)-1 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
		if err := a.store.FinishSendTask(task.ID, state, reason); err != nil {
			log.Printf("ERROR 保存发送结果失败: %v", err)
		}
		if state == "sent" {
			log.Printf("发送任务 #%d: %s", task.ID, state)
		} else {
			log.Printf("WARN 发送任务 #%d: %s", task.ID, state)
		}
	}
}

func (a *App) sendQueuedPart(ctx context.Context, part store.Message) (state, reason string) {
	// Create 与事件消费互斥，防止对象刚创建就被收信 worker 取走。
	a.activeMu.Lock()
	path, err := a.modem.Create(part.Peer, part.Body)
	if err == nil {
		a.activePaths[path] = true
	}
	a.activeMu.Unlock()
	if err != nil {
		log.Printf("WARN 创建发送分段 #%d 失败: %v", part.ID, err)
		_ = a.store.UpdateSendPart(part.ID, "", "failed", "模组创建短信失败")
		return "failed", "模组创建短信失败"
	}
	defer func() { a.activeMu.Lock(); delete(a.activePaths, path); a.activeMu.Unlock() }()
	if err := a.store.UpdateSendPart(part.ID, path, "sending", ""); err != nil {
		return "unknown", "持久化对象路径失败，未提交发送"
	}
	// --send 报错/超时也可能已被运营商接收，必须继续核对真实终态。
	sendErr := a.modem.Send(path)
	if sendErr != nil {
		log.Printf("WARN 提交发送分段 #%d 异常: %v", part.ID, sendErr)
	}
	state = "unknown"
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		sms, err := a.modem.Fetch(path)
		if err == nil && sms != nil {
			switch sms.State {
			case "sent", "delivered":
				state = "sent"
			case "failed":
				state = "failed"
			}
		}
		if state != "unknown" {
			break
		}
		select {
		case <-ctx.Done():
			reason = "进程退出，发送结果待确认"
			goto done
		case <-deadline.C:
			reason = "模组未返回最终状态，请核实后再决定是否重发"
			goto done
		case <-time.After(1500 * time.Millisecond):
		}
	}
done:
	if state == "failed" {
		reason = "模组报告发送失败"
	}
	if state == "unknown" && sendErr != nil {
		reason = "发送调用异常，结果待确认；请勿直接重发"
	}
	if err := a.store.UpdateSendPart(part.ID, path, state, reason); err != nil {
		return "unknown", "保存发送结果失败，保留模组副本"
	}
	if state == "sent" || state == "failed" {
		_ = a.modem.Delete(path)
	}
	return state, reason
}
