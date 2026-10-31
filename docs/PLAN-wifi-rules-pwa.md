# 实施方案：Wi-Fi 管理 / 转发规则路由 / PWA 主屏图标

> 2026-11-01 定稿。三项相互独立，按 Wi-Fi → 规则 → PWA 顺序实施，每项独立提交。
> 已核实的代码事实：服务以 root 运行（无 User=）；channels 表的 config 列是 JSON blob
> （`forward.Config` 整体存取）；`nmcli dev wifi list` 实测 1.5s 且不断连（NM 1.52）。

---

## 功能 1：Wi-Fi 管理

### 1.1 边界

- **做**：扫描周边 / 连接（含密码）/ 已保存网络管理与忘记 / 当前连接状态
- **不做**：隐藏网络、AP 热点模式、WPS、频段与国家码设置——v2 再议

### 1.2 新包 `internal/wifi/wifi.go`（shell out nmcli，零依赖）

- `Network{SSID, Signal, Security, Active bool}`
- `Scan()`：`nmcli -t -f SSID,SIGNAL,SECURITY,ACTIVE dev wifi list ifname wlan0`；
  过滤空 SSID（隐藏网络不展示）；同 SSID 多 BSS 保最强信号
- `Status()`：SSID/信号取 Scan 的 ACTIVE 行，IP/速率复用 sysinfo
- `Saved()`：`nmcli -t -f NAME,TYPE,DEVICE connection show`，按 TYPE==802-11-wireless 过滤，
  标记 ACTIVE
- `Connect(ssid, password)`：
  - 已有同名 profile → `nmcli con up <name>`（忽略传入密码——profile 里存的是旧密码，
    想换密码走先忘记再连接，语义清晰）
  - 否则 → `nmcli dev wifi connect <ssid> [password <pw>] ifname wlan0`
  - exec 超时 45s（含 DHCP）；空密码 = 开放网络
  - **安全底线：绝不删除原活跃 profile**。连错密码 NM 自动回连原网络，永远有退路
- `Forget(name)`：`nmcli con del <name>`；活跃连接由 API 层拒绝（409）
- 包内 `sync.Mutex` 串行化（单 wlan0，防扫描/连接并发互踩）
- **命令注入防护：SSID/密码一律走 exec.argv，绝不拼 shell 字符串**
- nmcli 不存在（如宿主机调试）→ 返回友好错误 "nmcli 不可用"，不 panic

### 1.3 API（自动继承 token 鉴权）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/wifi/status` | `{ssid, signal, ip}` |
| GET | `/api/wifi/scan` | `{items:[...]}`（NM 缓存，秒回） |
| POST | `/api/wifi/connect` | `{ssid, password}`，同步等结果（≤50s），`{ok}`/`{error}` |
| GET | `/api/wifi/saved` | `{items:[{name, active}]}` |
| DELETE | `/api/wifi/saved/{name}` | 活跃连接返回 409 `{error:"不能删除当前连接的配置"}` |

Backend 接口 + App 实现各 5 个。connect 阻塞期间不影响 SSE/其他请求。

### 1.4 Web UI（设置页「Wi-Fi」区）

- 当前连接卡：SSID + 4 档信号条 + IP +「重新扫描」
- 周边网络列表：信号条 + 锁图标；开放网点击直连，加密网弹密码框（与 401 输入框同交互习惯）
- 连接中：按钮转圈 + 文案「成功后设备 IP 可能变化；失败会自动回连原网络，短信不受影响」
- 已保存网络：非活跃的显示「忘记」（活跃的不显示按钮，后端 409 双保险）

---

## 功能 2：转发规则路由

### 2.1 语义（AND 组合，空 = 不限）

每通道 4 个条件，全部参与匹配：

| 条件 | 语义 | 典型值 |
|---|---|---|
| `match_peer` | 对端号码**前缀**，逗号分隔，任一命中 | `106,10086` |
| `match_keyword` | 正文关键词（substring），任一命中 | `验证码,动态码` |
| `match_skip_keyword` | 排除关键词，任一命中 → 跳过该通道 | `退订回T,推广` |
| `match_code_only` | 仅入站且提取到验证码的消息 | true |

- 用户故事落地：「验证码→钉钉」= 钉钉通道 code_only；「广告只入库」= 钉钉通道 skip=「推广,退订」
- **所有通道都不匹配 = 消息标记 forwarded**（无事可做，不算失败，不留待转发积压）

### 2.2 实现（零表迁移）

- `forward.Config` 加 4 字段（`MatchPeer/MatchKeyword/MatchSkipKeyword string`、
  `MatchCodeOnly bool`，全部 omitempty）——channels.config 是 JSON blob，
  Create/UpdateChannel 自动往返，**无需 ALTER TABLE、无需回填**
- `forward.Channel.Matches(v Vars) bool`：纯函数，表驱动测试
  （前缀命中/未命中、关键词任一、排除优先于关键词、code_only 对出站恒 false、全空=全过）
- `forwardBatch`：`DeliveryStatus` 检查之后、`Send` 之前 `if !ch.Matches(v) { continue }`；
  跳过不记 channel_deliveries、不计入 allOK 失败
- **fwd-test 旁路规则**（测连通性 ≠ 验证路由）；「重新转发」走规则（行为可预期）

### 2.3 API 与 UI

- `/api/channels` 系列零改动（Config 整体透传；新字段非敏感，脱敏逻辑不碰）
- 通道表单加「转发条件」区：三个输入框（hint：逗号分隔，留空不限）+ 仅验证码开关
- 通道列表卡片显示条件徽章：`106*`、`仅验证码`、`排除:推广`

---

## 功能 3：PWA 主屏图标

### 3.1 诚实约束（先说清楚）

完整 PWA 安装（WebAPK/独立窗口）需要 HTTPS 或 localhost。当前入口是 HTTP：

- **iOS Safari**：添加到主屏幕 HTTP 也可用——apple-touch-icon + meta 生效，独立窗口可开 ✅
- **Android Chrome**：HTTP 下只有「创建快捷方式」，无 manifest 级安装 ⚠️

结论：manifest/图标/meta 全套照做，HTTP 下 Android 体验打折是已知边界；
未来若上 HTTPS（Cloudflare DNS-01，token 都是现成的）自动升级为完整 PWA。
**不引入 service worker**（SW 同样要求 HTTPS，收益为零）。

### 3.2 交付

- 图标：AI 生成 512×512（绿色气泡 +「信」字，浅底，与 UI 同色系）→ 缩出 192；
  声明 `purpose: "any maskable"`；存 `internal/api/web/icons/`
- `internal/api/api.go`：go:embed 扩展 manifest.webmanifest 与 icons；路由加
  `/manifest.webmanifest` 与 `/icons/`（静态、免 token，与首页同级）
- `index.html` head：`<link rel="manifest">`、`theme-color`（对齐页面 --bg）、
  `apple-touch-icon`、`apple-mobile-web-app-capable` + `status-bar-style`、`description`
- manifest：`name 短信网关`、`start_url /`、`display standalone`、icons 192/512

---

## 实施顺序与验证

1. Wi-Fi 全栈 → commit
2. 规则路由（含 Matches 表驱动测试）→ commit
3. PWA（生成图标 → 嵌入 → meta）→ commit
4. 每步固定动作：`go test ./...` + `go vet` + `node --check` + DOM id 全量比对 + host 冒烟
   （Wi-Fi 包在宿主机 nmcli 缺失，主要验证 API 形状与错误路径；真连接必须设备实测）
5. 统一部署后设备实测清单：
   - 扫描出 LUOLUOM 且信号/IP 与 nmcli 手工结果一致
   - 故意连错密码 → 明确报错且原连接自动恢复、短信收发不受影响
   - 忘记一个非活跃已保存网络成功；对活跃连接的删除请求得 409
   - 规则：造一条 code_only 通道收验证码、一条 skip 通道被排除，`channel_deliveries` 与徽章显示正确
   - iOS 添加主屏，图标与独立窗口正常
6. `make privacy` 后推送

## 明确不做（本轮）

USSD 查余额（待实测立项）、数据导出/备份（下一轮）、HTTPS、隐藏网络、热点模式
