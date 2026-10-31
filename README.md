# SMSRelay8916

jz02v10（MSM8916 / 高通 410）随身 WiFi 的短信网关。收短信、归档、钉钉转发、Web 收发。

Go 重写版，替换早期 Python 实现（`/opt/sms-gw/sms-gw.py`）。API 与页面完全兼容，共用同一个 SQLite 归档库。

> 早期 Python 版已从设备上移除，运行期只依赖这一个静态二进制；`/usr/local/bin/sms` 命令行工具改为 `curl + jq`，不再需要 python3。

---

## 为什么用 Go

这台设备的瓶颈不是内存，是**每 3 秒 spawn 一次 `mmcli`**。Python 版没有 D-Bus 绑定，只能轮询子进程，常驻 CPU 约 2%；Go 版订阅 ModemManager 的 D-Bus 信号（`Messaging.Added` / `Deleted`），空闲时完全不产生子进程。

| 指标 | Python 版 | Go 版（实测） |
|---|---|---|
| 常驻内存 (RSS) | 38.3 MB | **13~15 MB** |
| 空闲 CPU | 0.7~2%（每 3 秒轮询） | **0.2~1%**（取决于是否撞上 60 秒兜底扫描） |
| 收取触发 | 每 3 秒轮询 | D-Bus 信号，实时 |
| 短信入库延迟 | 8~12 秒 | **8~11 秒** |
| 部署依赖 | python3 解释器 | 单个静态二进制 |
| 二进制大小 | — | **11 MB**（arm64，静态链接，stripped） |

内存绝对值差异在这台 371 MB 的机器上不算关键（`rmtfs` 占 47 MB、`NetworkManager` 20 MB），真正的收益是**事件驱动**和**部署干净**。

---

## 架构

```
        ┌──────────────── D-Bus（系统总线）────────────────┐
        │  org.freedesktop.ModemManager1.Modem.Messaging   │
        │  Added(o path, b received) / Deleted(o path)     │
        └───────────────────────┬──────────────────────────┘
                                │ 事件
                     ┌──────────▼──────────┐
                     │  events channel     │  串行 worker
                     │  （容量 512，满则丢）│
                     └──────────┬──────────┘
                                │
                ┌───────────────▼────────────────┐
                │  process(path) 状态机           │
                │  1. 读 mmcli -s <path> -J       │
                │  2. 中间态 → 2 秒后重投          │
                │  3. 正文须稳定 6 秒             │
                │  4. 内容哈希去重 → SQLite       │
                │  5. 确认入库后才删 modem 副本    │
                └───────────────┬────────────────┘
                                │
        ┌───────────────────────┼────────────────────────┐
        │                       │                        │
   ┌────▼─────┐          ┌──────▼──────┐          ┌──────▼──────┐
   │ SQLite   │          │ 转发 goroutine│          │ HTTP :8088  │
   │ (WAL)    │          │ 钉钉（限流）  │          │ API + Web UI│
   └──────────┘          └─────────────┘          └─────────────┘

兜底：每 60 秒全量扫描一次对象列表，防止漏信号。
```

### 为什么数据读取还用 `mmcli` 而不是纯 D-Bus

D-Bus 上的 `state` / `pdu-type` 是原始 `uint32` 枚举，需要自己维护映射表。**映射一旦搞错，就会把「接收中」误判成「已接收」，在长短信分片还没到齐时删掉对象 —— 永久丢短信（踩过）。**

`mmcli -J` 输出的已经是字符串（`deliver` / `received` / `submitting`），这条路径经过长期实测。所以：

- **事件通知**用 D-Bus（零轮询，这是性能收益来源）
- **数据读取**用 `mmcli -J`（枚举无歧义，避免重新踩坑）

代价是每条短信多一次 55ms 的子进程，但有事件才触发，空闲时为零。

---

## 目录

```
cmd/smsrelay/main.go          入口、配置、事件循环、状态机、发送、转发
internal/store/store.go       SQLite 归档（表结构与 Python 版一致）
internal/modem/modem.go       mmcli 封装 + 状态解析
internal/modem/watch.go       D-Bus 信号订阅
internal/relay/dingtalk.go    钉钉机器人（限流 + 重试）
internal/api/api.go           HTTP API + 内置页面
internal/api/web/index.html   Web UI（go:embed）
deploy/                       部署用 systemd 单元与配置样例
build.sh                      交叉编译（linux/arm64，静态）
deploy.sh                     推送到设备并切换服务
```

---

## 构建

在 WSL（Ubuntu 24.04）里：

```bash
cd ~/github/SMSRelay8916
./build.sh                    # 交叉编译 linux/arm64 → dist/smsrelay-linux-arm64
./build.sh --install-go       # 没装 Go 时先 apt 装
./build.sh --host             # 编本机架构，便于在 x86 上调试逻辑
```

脚本会：

1. 检查 Go ≥ 1.21
2. 探测 `goproxy.cn` 直连是否可用；不通时才使用 `SMSRELAY_PROXY` 指定的代理（脚本**不内置任何代理地址**，`--no-proxy` 强制直连）
3. 首次运行 `go get` 拉 `godbus/dbus/v5` 与 `modernc.org/sqlite`
4. `CGO_ENABLED=0` 编译静态二进制

`CGO_ENABLED=0` 是关键：`modernc.org/sqlite` 是纯 Go 实现，关掉 cgo 后**不需要任何交叉 C 工具链**，一条命令出二进制。这也是当初选它而不是 `mattn/go-sqlite3` 的原因。

## 部署

```bash
DEVICE_PASS=<设备密码> ./deploy.sh <设备地址>
```

脚本会：校验架构 → 上传 → 装到 `/opt/smsrelay/` → 沿用已有配置（优先 `/etc/smsrelay/config.json`，其次老版本的 `/etc/sms-gw/config.json`）→ 停掉遗留的 `sms-gw`（若存在）→ 启 `smsrelay` → 自检。

回退到上一个二进制：

```bash
# 覆盖前先备份：cp /opt/smsrelay/smsrelay /opt/smsrelay/smsrelay.bak
systemctl stop smsrelay
cp /opt/smsrelay/smsrelay.bak /opt/smsrelay/smsrelay
systemctl start smsrelay
```

## 配置

默认读 `/etc/smsrelay/config.json`（可用 `SMSRELAY_CONFIG` 覆盖）；文件不存在时会尝试老版本留下的 `/etc/sms-gw/config.json`，两者字段名完全一致。

```json
{
  "listen_host": "0.0.0.0",
  "listen_port": 8088,
  "api_token": "",
  "db_path": "/var/lib/smsrelay/sms.db",
  "guard_interval": 60,
  "modem_index": "0",
  "web_dir": "/opt/smsrelay/web",
  "own_number": "13800000000",
  "dingtalk": {
    "enabled": true,
    "url": "https://oapi.dingtalk.com/robot/send?access_token=...",
    "prefix": "【短信】",
    "max_per_min": 15
  }
}
```

- `api_token` 为空表示不鉴权；非空时需 `X-Token` 头或 `?token=`
- `web_dir` 存在时优先用外部 `index.html`，改页面不用重编译
- `guard_interval` 是兜底轮询秒数，设 0 用默认 60
- 上面所有值都是**占位值**：`own_number` 换成你自己的号码（留空则跟随 SIM 卡），`access_token` 换成真实 token

## 运行期组件

| 组件 | 路径 | 作用 |
|---|---|---|
| `smsrelay.service` | `/opt/smsrelay/smsrelay` | 主进程：收信 / 归档 / 转发 / HTTP |
| `smsrelay-keepalive.service` | `/usr/local/bin/smsrelay-keepalive.sh` | 每 10 秒一个 ICMP，避免 LTE 停在空闲态导致收不到寻呼 |
| `smsrelay-maint.timer` → `.service` | `/usr/local/bin/smsrelay-maint.sh` | 每天 04:10 停 MM 独占 AT 口，清掉孤儿短信分片（纯 shell，不依赖 python3） |
| `sms` | `/usr/local/bin/sms` | 命令行工具，只依赖 `curl` + `jq` |

```bash
sms status              # 号码 / 信号 / 收发统计 / 投递延迟
sms list 20             # 最近 20 条
sms send 10086 cxye     # 发送（超长自动拆分）
sms fwd-test            # 向所有启用通道发测试消息
```

数据与配置都在 root 下，注意备份：

```
/var/lib/smsrelay/sms.db     归档库（含短信、通道、DDNS、设置）
/etc/smsrelay/config.json    配置（含 token，权限 600）
```

## 转发通道与模板

通道在界面上配置（`/api/channels`），支持 `dingtalk` 与 `webhook` 两种类型，可同时启用多条。
每条通道的投递结果独立记账，一条失败不会让其它通道重推。

模板占位符（纯文本用不带后缀的，JSON 里用 `_json` 后缀）：

| 占位符 | 含义 | `_json` 版本 |
|---|---|---|
| `{{own_number}}` | 本机号码（收信号码） | `{{own_number_json}}` |
| `{{number}}` | 对方号码 | `{{number_json}}` |
| `{{body}}` | 短信正文 | `{{body_json}}` |
| `{{ts}}` | 时间 | `{{ts_json}}` |
| `{{dir}}` | `in` / `out` | `{{dir_json}}` |
| `{{id}}` | 归档记录 id | — |
| `{{prefix}}` | 通道配置的前缀 | `{{prefix_json}}` |

**`_json` 版本自带引号**，所以 JSON 模板里直接写 `"text": {{body_json}}`，
不要再套一层引号（写成 `"text": "{{body_json}}"` 会得到双引号嵌套的非法 JSON）。

webhook 的正文模板留空时，使用下面这个合法 JSON 默认模板：

```json
{"own_number":{{own_number_json}},"from":{{number_json}},"text":{{body_json}},"ts":{{ts_json}},"id":{{id}},"dir":{{dir_json}}}
```

## 安全与隐私

这个仓库里**不含任何真实凭证或个人数据**，并且刻意做了以下约束：

| 项 | 做法 |
|---|---|
| `config.json`（含 token、本机号码） | 在 `.gitignore` 里，永不入库；只存在设备本地 |
| 钉钉 / Cloudflare Token | 只出现在设备上的配置文件与数据库；接口返回一律脱敏成 `****1234` |
| 设备地址、密码、代理 | 脚本不内置，全部通过参数或环境变量传入 |
| 文档里的号码 / IP / IPv6 | 全部为占位值（`13800000000`、`192.168.1.100`、`203.0.113.10`、`2409:xxxx:...`） |

部署到公网前请自行加固：设置 `api_token`、把 8088 只监听内网或套反向代理加 TLS、
不要把 `/api/settings` 与 `/api/ddns` 直接暴露到公网（它们能读到脱敏后的 token 前缀与全部配置）。

提交前跑一次自检（会同时扫工作区和全部 git 历史）：

```bash
make privacy
```

想让它自动拦截，把下面这行写进 `.git/hooks/pre-commit`：

```bash
#!/bin/sh
exec ./scripts/privacy-check.sh
```

## HTTP API

与 Python 版完全一致。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/ping` | 存活检查 |
| GET | `/api/status` | modem 状态、SIM、统计、寻呼延迟 |
| GET | `/api/messages?limit=30&offset=0&dir=in&q=关键词` | 分页查询 |
| POST | `/api/send` | `{"number":"10086","text":"cxye"}` |
| POST | `/api/forward-test` | 钉钉连通性测试 |
| POST | `/api/forward` | `{"id":N}` 重新入队 |
| DELETE | `/api/messages/{id}` | 删除归档记录 |

---

## 关键设计决策（都是踩过的坑，改代码前先读）

### 1. 短信存储只有 73 个槽位，满了会静默丢短信

```
AT+CPMS?  →  "ME",0,23  "SM",0,50
```

modem `ME` 23 条 + SIM `SM` 50 条，在通信处理器里写死，**无法扩容**。槽满后短信中心直接丢弃新短信，不报错也收不到。

所以必须「收到即取走」：归档 → 立即删 modem 副本 → 槽位永远空着。这是整个程序的核心约束。

### 2. 绝不能用 SMS 对象路径做幂等键

ModemManager **回收复用**对象路径：删掉 `SMS/3` 后，下一条新短信还会被命名成 `SMS/3`。

早期实现用 `mm_path` 判重，于是新短信被误判为「已记录」，跳过入库却照样删除对象 —— **消息静默销毁，库里连痕迹都没有**。

现在身份只由 `sha1(pdu类型|号码|时间戳|正文)` 决定，`mm_path` 仅作记录。

### 3. 只有确认入库才允许删除

`Archive()` 返回 `true` 才调 `Delete()`。失败就打 WARN 留在原处，5 秒后重试。

结构上杜绝「删了但没存」。

### 4. 长短信分片未到齐时不能取走

长短信是多条 PDU 陆续到达，MM 拼进同一个 SMS 对象。若在一片刚到、下一片还在路上时删掉对象，剩下一片会留在 SIM 里变成孤儿，之后短信中心复用相同分片序号就撞车（MM 日志：`A part with index 1 was already taken`），整条永久丢失。

两道闸：

- 状态是 `sending` / `receiving` / `draft` / `unknown` / 空 / `--` → 一律跳过，最多等 30 秒兜底
- 正文必须**稳定 6 秒**（`holdSeconds`）才归档

### 5. 绝不直接打开 `/dev/wwan0atN`

ModemManager 自己就是用 `wwan0at1` 下发 `AT+CMGR` 把 `+CMTI` 通知变成短信对象的。任何外部程序去同一个口发 AT（哪怕只是 `AT+CPMS?`）都会打断它的会话。

现象极其迷惑：**`+CMTI` 照常到达、短信也确实在 SIM 里，但永远不变成 SMS 对象 —— 表现为「收不到短信」**。

本程序完全不碰 AT 端口。要看存储占用必须先 `systemctl stop ModemManager`。

### 6. 发送正文必须走临时文件

`mmcli --messaging-create-sms` 用逗号分隔 `key=value`，正文里只要有逗号就把参数解析炸掉，换行和引号也不可靠。

必须：`--messaging-create-sms=number=X` + `--messaging-create-sms-with-text=<临时文件>`。

### 7. systemd 单元不能依赖 `network-online.target`

设备有 wlan0 + wwan0 双默认路由，`NetworkManager-wait-online` 会长期不返回，导致服务开机被无限期排队。

只 `After=ModemManager.service`。

### 8. 别频繁重启 ModemManager

每次重启都会丢 SGs 关联，之后几分钟的短信全要排队重投。诊断时优先用 D-Bus 监听（`gdbus monitor`），不要重启服务。

### 9. 空闲态收不到短信（机型固有限制）

这台 MSM8916 + 2015 年 MPSS 固件在 LTE 空闲态（ECM-IDLE）下收不到网络寻呼。别人随机时间发来的短信可能延迟几分钟，直到设备因其它原因重新附着才补投。

`AT+CREG: 0,1` 显示 CS 域已注册，但空闲寻呼实际打不到，属固件/网络侧行为，软件改不动根因，只能缓解：`smsrelay-keepalive.service` 每 10 秒 ping 一次，维持连接活跃（约 0.5 MB/天）。

`/api/status` 的 `paging_24h` 字段用于观察缓解效果：`slow_count`（延迟 > 60s 的条数）应为 0。

---

## 已知限制

- 单进程单实例，SQLite 用 WAL，并发写靠 `busy_timeout`
- 钉钉机器人限流 20 条/分钟，程序按 `max_per_min`（默认 15）节流
- 只处理文本短信；数据短信（`data`）归档为 `[非文本内容]`
- 发送状态轮询最长 45 秒，超时按「已发送」处理

## 实测记录（2026-10-31，jz02v10）

部署后逐项验证：

| 项 | 结果 |
|---|---|
| 交叉编译 | `aarch64` 静态二进制 11 MB，`go vet` 零告警 |
| 冷启动 | 服务自启，p12/归档库/归档记录全部保留 |
| 发送（含逗号+换行） | 1 秒完成，`state: sent`，正文原样保留 |
| 超长拆分 | 124 字符 → 2 条，带 `(1/2)(2/2)` 标记，均 sent |
| 入站收取 | 10~22 秒入库，长回复 283 字完整重组 |
| 钉钉转发 | `forwarded #N`，旧记录 `fw=1` |
| 槽位释放 | 每轮结束 `No sms messages were found` |
| 重启恢复 | uptime 48s 时服务已 active、modem connected |
| 空闲资源 | RSS 13~15 MB，CPU 0.2~1% |

关于 CPU：同一套代码在不同采样窗口会在 0.2%~1% 之间波动，差异主要来自是否撞上
每 60 秒一次的兜底扫描（每次 spawn 一个 `mmcli`，约 55 ms）。试过 `GOMAXPROCS=1`
与 `GOGC=200`，**都没有稳定改善**（同一配置重复测量噪声大于差异），因此不做这类调优。

还可以进一步压低：把 `guard_interval` 从 60 调到 300，CPU 会降到 0.2% 左右，
代价是漏信号时最坏恢复时间从 1 分钟变成 5 分钟。当前保持 60 秒，优先保证及时性。


## 许可

内部使用。
