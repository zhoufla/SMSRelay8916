// Package api 定义短信后台与本地 RPC 共用的数据结构，不提供 HTTP 服务。
package api

import "github.com/zhoufla/SMSRelay8916/internal/store"

type GatewayInfo struct {
	Uptime   int    `json:"uptime"`
	Hostname string `json:"hostname"`
	DingTalk bool   `json:"dingtalk"`
	Version  string `json:"version"`
}

type StatusPayload struct {
	Modem           interface{}  `json:"modem"`
	OwnNumber       string       `json:"own_number"`
	OwnNumberSource string       `json:"own_number_source"`
	Counts          store.Counts `json:"counts"`
	Today           store.Today  `json:"today"`
	LastInbound     string       `json:"last_inbound"`
	Paging          store.Paging `json:"paging_24h"`
	Gateway         GatewayInfo  `json:"gateway"`
	DatabaseError   string       `json:"database_error,omitempty"`
}
