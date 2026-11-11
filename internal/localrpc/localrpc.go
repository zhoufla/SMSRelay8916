// Package localrpc 提供仅供本机 rpcd 使用的、有界 JSON Unix Socket 协议。
package localrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const Socket = "/var/run/smsrelay/control.sock"
const MaxRequest = 64 * 1024

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type Response struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

type Handler func(string, json.RawMessage) (interface{}, error)

// Listen 不覆盖正在使用的 Socket；残留文件只允许是 Unix Socket。
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("RPC 路径不是 Socket")
		}
		c, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			c.Close()
			return nil, fmt.Errorf("RPC 已在运行")
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func Serve(ctx context.Context, l net.Listener, handler Handler) error {
	go func() { <-ctx.Done(); l.Close() }()
	slots := make(chan struct{}, 8)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { c.Close(); <-slots }()
				_ = c.SetDeadline(time.Now().Add(25 * time.Second))
				var req Request
				dec := json.NewDecoder(io.LimitReader(c, MaxRequest+1))
				var resp Response
				if err := dec.Decode(&req); err != nil || dec.InputOffset() > MaxRequest {
					resp.Error = "请求格式错误或过大"
				} else {
					data, err := handler(req.Method, req.Params)
					if err != nil {
						resp.Error = err.Error()
					} else {
						resp.OK = true
						resp.Data = data
					}
				}
				_ = json.NewEncoder(c).Encode(resp)
			}()
		default:
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			_ = json.NewEncoder(c).Encode(Response{Error: "后台繁忙，请稍后重试"})
			c.Close()
		}
	}
}

// Call 的调用进程由 rpcd 按需启动，不读取 UCI 或打开数据库。
func Call(path string, req Request) (json.RawMessage, error) {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("短信后台未运行")
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(26 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := json.NewDecoder(io.LimitReader(c, 2*1024*1024)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
