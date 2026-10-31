# 功能规划：系统监控 / 实时推送 / 通道配置 / DDNS

> 状态：**已确认，待实施**
> 目标设备：jz02v10（MSM8916，4×A53 @998MHz，371MB RAM，3.3GB eMMC，Debian 13）
>
> 已确认的决策（2026-10-31）：
> 1. **实时推送只做 SSE**，不做 MQTT（页面场景已够用，避免维护 broker）
> 2. **DDNS 的 A / AAAA 都实现且均可选**，由用户自由组合（只配 V4、只配 V6、或两者都配）
> 3. **API 鉴权告警但不强制**（token 为空时页面红条提示，不锁死自己）

---

## 一、先看设备实际能力（已实测）

这些数据决定了后面的方案取舍，先列出来。

### 1.1 温度

有 6 个 hwmon 传感器，读取 `/sys/class/hwmon/hwmonN/temp1_input`（毫摄氏度）：

| hwmon | name | 当前值 |
|---|---|---|
| hwmon0 | （未命名） | 49.0 °C |
| hwmon1 | camera_thermal | 46.0 °C |
| hwmon2 | gpu_thermal | 43.0 °C |
| hwmon3 | cpu2_3_thermal | 47.0 °C |
| hwmon4 | cpu0_1_thermal | 46.0 °C |
| hwmon5 | pm8916_thermal | 46.6 °C |

`thermal_zone0` 的 policy 是 `step_wise`，说明有温控降频。CPU 频率档位 `200/400/800/998.4 MHz`，governor `schedutil`。

### 1.2 网络：这是 DDNS 方案的关键约束

```
wlan0   192.168.1.100/24
        2409:xxxx:xxxx:xxxx::1/128              ← 公网 IPv6（DHCPv6）
        2409:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx/64 ← 公网 IPv6（SLAAC）
wwan0   10.64.0.10/29                          ← CGNAT，非公网 IPv4
        2409:yyyy:yyyy:yyyy:yyyy:yyyy:yyyy:yyyy/64 ← 公网 IPv6，但不可用
```

实测结论：

| 测试 | 结果 | 含义 |
|---|---|---|
| `ping6 2400:3200::1` | 通 | IPv6 出站可用 |
| `curl -6 https://www.taobao.com` | 200，0.21s | IPv6 TCP 正常 |
| `ip -6 route get` 源地址 | `2409:xxxx:xxxx:xxxx::1` | **走 wlan0** |
| 绑定 wwan0 源 `ping6` | 不通 | 4G 的 IPv6 是死地址 |
| `ping6` wwan0 的 v6 网关 | 不通 | 连第一跳都不可达 |
| IPv4 出口 `api.ipify.org` | 203.0.113.10 | 运营商 CGNAT 出口 |
| `ping6 -s 1400` | 通 | 路径 MTU ≥1400 |
| `ping6 -s 1452` | 不通 | 路径 MTU 在 1400~1452 之间 |

**三条硬结论：**

1. **IPv4 没有公网入口**。`10.64.x.x` 是 CGNAT，出口 `203.0.113.10` 是运营商共享地址，无法映射到本机。**A 记录填上去，外部也连不进来。**
2. **wwan0 的 IPv6 是"只发地址不开数据面"**，不能用作 AAAA。
3. **只有 wlan0 的 IPv6 真正可用**，且需家庭路由器放行入站。

### 1.3 其他

- 内存 371MB，可用 255MB，另有 185MB swap（当前未用）
- 磁盘 3.3GB / 已用 1.1GB（35%）
- `/sys/class/net/wwan0/statistics/{rx,tx}_bytes` **恒为 0**（驱动未实现），wlan0 正常
- 无 mosquitto，无 MQTT 端口监听
- 无 `nft`，有 `ip6tables`（3 条规则），内核 IPv6 转发关闭

---

## 二、功能设计

### 功能 1：系统指标

**采集方式**：纯 Go 读 procfs/sysfs，零 CGO、零外部命令。

| 指标 | 来源 | 说明 |
|---|---|---|
| CPU 使用率 | `/proc/stat` 两次采样差值 | 需保存上次快照，首次返回 null |
| 每核使用率 | 同上（cpu0..cpu3） | 可选，前端画 4 个小条 |
| 负载 | `/proc/loadavg` | 1/5/15 分钟 |
| 当前频率 | `/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq` | kHz → MHz |
| 内存/swap | `/proc/meminfo` | 取 MemTotal/MemAvailable/Cached/Buffers |
| 温度 | `/sys/class/hwmon/hwmon*/temp1_input` | 毫摄氏度 ÷1000，取 cpu0_1 为主，另附 pmic/gpu |
| 磁盘 | `syscall.Statfs("/")` | 总/用/可用/百分比 + inode |
| 网络速率 | `/sys/class/net/*/statistics/*` | wlan0 可用；wwan0 恒 0，标注"驱动不支持" |
| 运行时长 | `/proc/uptime` | |
| 进程数 | `/proc/loadavg` 第 4 段 | running/total |

**采样策略**：后台 goroutine 每 5 秒采一次，结果放原子变量。HTTP 请求直接读缓存，**不产生额外开销**。

> 为什么不在请求时采：CPU 使用率需要两次采样间隔（至少 1 秒），请求时采会让响应变慢 1 秒。

**API**

```http
GET /api/system
```
```json
{
  "cpu":  {"percent": 12.5, "per_core": [8.1, 15.2, 10.0, 16.7], "load": [0.16, 0.10, 0.09],
           "freq_mhz": 800, "cores": 4},
  "mem":  {"total_mb": 371, "used_mb": 115, "available_mb": 255, "cached_mb": 161,
           "swap_total_mb": 185, "swap_used_mb": 0},
  "temp": {"cpu_c": 46.0, "gpu_c": 43.0, "pmic_c": 46.6, "max_c": 49.0,
           "sensors": [{"name": "cpu0_1_thermal", "c": 46.0}]},
  "disk": {"path": "/", "total_gb": 3.3, "used_gb": 1.1, "free_gb": 2.1, "percent": 35},
  "net":  {"wlan0": {"rx_bps": 1234, "tx_bps": 567, "available": true},
           "wwan0": {"available": false, "note": "驱动未实现统计"}},
  "uptime_s": 4607,
  "procs": {"running": 1, "total": 162},
  "ts": "2026-10-31T21:15:00+08:00"
}
```

**前端**：状态页顶部 4 个横向卡片（CPU / 内存 / 温度 / 磁盘），下面一张网络 + 运行时长的表。移动端 2×2 网格。

---

### 功能 2：实时推送

先明确一件事：**页面和外部集成是两个不同场景，不该用同一个方案。**

| 方案 | 额外依赖 | 每连接内存 | 适用 |
|---|---|---|---|
| **SSE** | 无（`net/http` + `Flusher`） | ~8 KB | 页面实时更新 ✅ |
| WebSocket | `gorilla/websocket` | ~16 KB | 需要浏览器→服务器双向（当前无） |
| MQTT | broker + `paho.mqtt.golang` | broker ~8 MB + 客户端 ~4 MB | 外部系统（HA/Node-RED） |

**推荐：SSE 做页面，MQTT 做成可选的外部发布，两者共存。**

理由是 SSE 对这个场景是严格更优解：单向（服务器推）、浏览器原生重连、无需握手库、断线不影响业务。为页面引入 MQTT 反而要额外维护 broker。

#### 2.1 SSE（页面用）

```http
GET /api/events?token=xxx
Content-Type: text/event-stream
```

事件类型：

| event | 频率 | 内容 |
|---|---|---|
| `hello` | 连接时一次 | 当前 status + 最近 1 条短信 |
| `system` | 5 秒 | 功能 1 的指标 |
| `sms` | 事件触发 | 新入站/出站短信 |
| `modem` | 状态变化时 | registration/signal 变化 |
| `ping` | 30 秒 | 注释行心跳，防代理断开 |

实现要点：
- 每个连接一个 goroutine + `chan []byte`（容量 16，满了丢弃并标记"客户端太慢"）
- 写超时 10 秒，失败即关闭连接
- `X-Accel-Buffering: no` 关掉可能的代理缓冲
- 前端 `EventSource` 自动重连；连接数上限 8（防误用）

#### 2.2 MQTT：不做（已确认）

页面场景用 SSE 已完全覆盖，外部集成暂时没有需求。引入 MQTT 需要额外维护 broker
（设备上没有 mosquitto，装一套 +8MB 内存 +2MB 磁盘），收益不匹配成本。

**如果将来要接**（Home Assistant / Node-RED），预留两个接入点即可，不需要改现有架构：
- `eventbus` 已经把所有事件（sms / system / modem）集中发布，加一个 MQTT 订阅者即可
- 主题建议 `smsrelay/{device_id}/sms`（QoS 1）、`/status`（QoS 1 + retain）、`/system`（QoS 0）

---

### 功能 3：转发通道（界面可配）

现在钉钉配置写死在 `config.json`，改一次要重启。改成多通道 + 热更新。

**数据模型**

```sql
CREATE TABLE channels (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL,
  type       TEXT NOT NULL,             -- dingtalk | webhook
  enabled    INTEGER DEFAULT 1,
  config     TEXT NOT NULL,             -- JSON，见下
  created_at TEXT DEFAULT (datetime('now','localtime'))
);
```

`config` 结构：

```jsonc
// type=dingtalk
{"url": "https://oapi.dingtalk.com/robot/send?access_token=xxx",
 "secret": "",              // 可选，钉钉"加签"用 HmacSHA256
 "prefix": "【短信】",
 "at_mobiles": []}

// type=webhook
{"url": "https://example.com/hook",
 "method": "POST",
 "headers": {"X-Token": "abc"},
 "content_type": "application/json",
 "body_template": "{\"from\":\"{{number}}\",\"to\":\"{{own_number}}\",\"text\":{{body_json}},\"at\":\"{{ts}}\"}"}
```

模板占位符：`{{number}}` 发送方、`{{own_number}}` 本机号、`{{body}}` 正文、`{{body_json}}` 正文（JSON 转义，可直接嵌进 JSON）、`{{ts}}`、`{{id}}`、`{{dir}}`。

**安全**：`GET /api/channels` 返回时把 URL 里的 `access_token=...`、`headers` 里的敏感值替换为 `****`，只保留后 4 位。PUT 时如果传入值仍是 `****` 结尾则保留原值。

**投递语义**（与现有逻辑一致，不能退化）：
- 每个通道独立队列，`attempts < 3`
- 成功标记，失败记录 `last_error`
- 任一通道成功即视为该条已转发（`forwarded=1`），但每个通道各自的成功状态单独记录在 `channel_deliveries` 表

```sql
CREATE TABLE channel_deliveries (
  message_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  status     TEXT,          -- pending | sent | failed
  attempts   INTEGER DEFAULT 0,
  last_error TEXT,
  updated_at TEXT,
  PRIMARY KEY (message_id, channel_id)
);
```

**API**

```
GET    /api/channels
POST   /api/channels
PUT    /api/channels/{id}
DELETE /api/channels/{id}
POST   /api/channels/{id}/test     → 发一条测试消息
```

**鉴权（重要）**：现在 `/api/*` 无鉴权。加入 DDNS（Cloudflare token）和通道配置（钉钉 token）后，**内网任何人都能读到或改掉**。本方案要求：
- `api_token` 非空时全部 `/api/*` 需要 `X-Token` 或 `?token=`
- 首次进入设置页若 token 为空，页面顶部红条提示"未设置访问口令"
- 不强制（避免锁死自己），但明确告警

---

### 功能 4：DDNS（Cloudflare）

**先把限制说清楚**（基于第一节的实测）：

| 记录类型 | 在本机上是否有意义 | 原因 |
|---|---|---|
| A（IPv4） | **基本没有** | 设备在 CGNAT 后面，解析出来的是运营商出口 `203.0.113.10`，外部连不进来 |
| AAAA（IPv6） | **有，但仅限 wlan0** | wlan0 的 `2409:xxxx:...` 是公网地址且数据面通；仍需要家庭路由器放行入站 |
| AAAA（wwan0） | **没有** | 4G 的 IPv6 网关不可达 |

所以实现上仍然支持 A 和 AAAA，但**页面必须明确显示可达性判断**，避免配完了发现连不上。

**数据模型**

```sql
CREATE TABLE ddns (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,       -- sub.example.com（完整记录名）
  type        TEXT NOT NULL,       -- A | AAAA
  zone_id     TEXT,                -- 留空则按域名自动查
  api_token   TEXT NOT NULL,       -- Cloudflare API Token（Zone.DNS:Edit）
  source      TEXT NOT NULL,       -- iface:wlan0 | iface:wwan0 | external
  proxied     INTEGER DEFAULT 0,
  ttl         INTEGER DEFAULT 1,   -- 1=auto
  enabled     INTEGER DEFAULT 1,
  last_ip     TEXT,
  last_update TEXT,
  last_error  TEXT
);
```

**取 IP 的策略**

| 来源 | 实现 | 适用 |
|---|---|---|
| `iface:wlan0` | 读 `net.InterfaceByName`，过滤 `IsGlobalUnicast()` 且非 ULA(fc00::/7)，优先 `/128` 那条 | AAAA，本机推荐 |
| `iface:wwan0` | 同上 | AAAA，但本机实测不可用 |
| `external` | IPv4: `https://api.ipify.org`；IPv6: 支持自定义 URL | A 记录、或 NAT 后场景 |

> 国内的 `ipify`/`icanhazip` 的 IPv6 端点在本机不通（实测 000），所以 IPv6 外部查询的 URL 要允许自定义。

**同步流程**

1. 取 IP → 与 `last_ip` 比对，相同则跳过（省 API 调用）
2. 解析 zone：`GET /client/v4/zones?name=example.com`（从记录名取后两段，需处理 `.com.cn` 这类二级后缀，简化方案：允许用户直接填 zone_id）
3. 查记录：`GET /client/v4/zones/{zid}/dns_records?type=AAAA&name=sub.example.com`
4. 存在则 `PUT .../dns_records/{rid}`，不存在则 `POST .../dns_records`
5. 更新 `last_ip` / `last_update`，失败写 `last_error`

**频率**：每 5 分钟检查一次（可配），IP 不变时不调用 API。

**可达性提示**：页面给每条记录显示一个徽章
- `源地址是 CGNAT (10/8, 172.16/12, 192.168/16, 100.64/10)` → 🔴 "无公网入口"
- `源是 wwan0 的 IPv6 且网关不可达` → 🔴 "4G IPv6 未开通"
- `源是 wlan0 的 IPv6` → 🟡 "公网地址，需路由器放行入站"

**API**

```
GET    /api/ddns
POST   /api/ddns
PUT    /api/ddns/{id}
DELETE /api/ddns/{id}
POST   /api/ddns/{id}/sync      → 立即同步一次
```

---

### 功能 5：转发内容带本机号码

当前格式：

```
【短信】 收到短信
号码：+8613900000001
内容：xxx
时间：2026-10-31 20:15:16
```

改为：

```
【短信】收到短信
收信号码：13800000000
发送号码：+8613900000001
内容：xxx
时间：2026-10-31 20:15:16
```

规则：
- 本机号码取自功能 6 的配置，回退到 SIM 的 `own-numbers`，都没有则显示 `未知`
- 做成**模板**，默认模板如上，用户可在通道配置里改（webhook 本来就要模板）
- 模板变量统一：`{{own_number}}`、`{{number}}`、`{{body}}`、`{{ts}}`

---

### 功能 6：本机号码可配置

```sql
-- settings 表通用键值
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT DEFAULT (datetime('now','localtime'))
);
```

键：`own_number`。

**优先级**（从高到低）：
1. `settings.own_number`（手动设置）
2. `config.json` 的 `own_number`（兼容现有部署）
3. ModemManager 的 SIM `own-numbers`
4. 空 → 显示 `未知`

API 返回时带上来源，便于排查：

```json
{"own_number": "13800000000", "source": "manual"}   // manual | config | sim | none
```

**API**

```
GET /api/settings
PUT /api/settings          {"own_number": "13800000000"}
```

页面：设置页一个输入框 + "当前生效值 / 来源"提示 + 一个"从 SIM 重新读取"按钮。

---

## 三、页面结构调整

现在单页 320 行，加 4 个模块后约 900~1100 行。建议拆成多文件（`index.html` + `app.js` + `style.css`，用 `go:embed web/*` 全量嵌入），并改成 **Tab 布局**：

```
┌──────────────────────────────────────┐
│ jz02 · 13800000000 · CMCC ·  [●] 在线 │  ← 顶栏（含 SSE 连接指示灯）
├──────────────────────────────────────┤
│  短信  │  系统  │  设置  │  DDNS      │  ← Tab
├──────────────────────────────────────┤
│                                      │
│  （对应内容）                          │
│                                      │
└──────────────────────────────────────┘
```

- **短信**：现有列表 + 发送框（不变）
- **系统**：4 个指标卡片 + 网络表 + 实时刷新（SSE `system` 事件）
- **设置**：本机号码、转发通道列表（增删改测）、MQTT（可选）
- **DDNS**：记录列表（增删改）+ 可达性徽章 + 立即同步

移动端：Tab 改为底部固定导航，卡片 2×2。

---

## 四、已确认的决策

| 决策 | 结论 | 影响 |
|---|---|---|
| 实时推送 | **只做 SSE**，不做 MQTT | 省约 400 行代码与 8MB 内存；`eventbus` 保留扩展点 |
| DDNS 记录类型 | **A / AAAA 都实现，均可选** | 三种组合都支持：只 V4、只 V6、两者都配 |
| API 鉴权 | **告警但不强制** | `api_token` 为空时页面顶部红条提示，接口照常可用 |

### DDNS 的组合方式

一条 `ddns` 记录 = 「一个域名 + 一种记录类型」。同一个域名可以建两条（一条 A、一条 AAAA），
也可以只建一条。程序不做约束，只给可达性判断：

| 组合 | 本机场景 | 提示 |
|---|---|---|
| 只 AAAA | 推荐 | 🟡 wlan0 的 IPv6 真实可用，仍需路由器放行入站 |
| 只 A | 本机无意义（CGNAT） | 🔴 源是 CGNAT，外部不可达 |
| A + AAAA | 双栈环境 | 分别提示 |

`source` 字段决定取哪个 IP，所以 V4/V6 完全解耦：A 记录一般用 `external`（查出口 IP），
AAAA 用 `iface:wlan0`。也允许把 A 指向 `iface:wlan0` 的私有地址（内网 DNS 场景）。

## 五、实施顺序

按依赖关系排，每阶段都能独立验证、独立上线。

| 阶段 | 内容 | 预估 |
|---|---|---|
| **P0** ✅ | `settings` 表 + 本机号码配置（功能 6）、系统指标采集与 `/api/system`（功能 1） | 已完成 |
| **P1** ✅ | SSE 事件总线 + 页面实时刷新（功能 2） | 已完成 |
| **P2** | 多通道转发 + 界面配置 + 模板化（功能 3、5） | 1 天 |
| **P3** | DDNS（Cloudflare，A/AAAA 可选）（功能 4） | 1 天 |
| **P4** | 页面重构为 Tab + 移动端适配 | 半天 |

~~P5 MQTT~~ —— 已确认不做。

每个阶段结束都跑一遍回归：发短信 → 入站 → 归档 → 转发 → 槽位清空。

**向后兼容**：`config.json` 继续可用。首次启动把 `config.json` 里的 `dingtalk` 配置迁移成一条 `channels` 记录（只迁移一次，用 `settings.migrated_dingtalk` 标记）。这样升级不需要手工重配。

---

## 六、风险与已知取舍

1. **DDNS 的实际价值受网络环境限制**，已在上文标注。不建议在没有公网入口的情况下指望它。
2. **wwan0 流量统计不可用**（驱动未实现 `tx_bytes`），系统页该卡片会显示"不可用"而不是假的 0。
3. **SSE 长连接**：每个打开的页面占一个 goroutine。限制 8 个连接，超出返回 503。
4. **温度传感器命名不统一**（有 `cpu0_1_thermal`、`pm8916_thermal`），代码按 `name` 匹配而不是固定索引，换机型时不会错位。
5. **Cloudflare 二级后缀**（`.com.cn` 等）解析 zone 会失败，所以 `zone_id` 允许手动填。
6. **配置热更新**：通道和 DDNS 改动立即生效（读内存），不需要重启服务；`listen_port`、`db_path` 这类启动参数仍需重启。
7. **不引入前端框架**：保持原生 JS，避免构建工具链。代价是页面代码会到 1000 行左右，用 Tab 拆分 + 统一的 `api()` 封装来控制复杂度。
