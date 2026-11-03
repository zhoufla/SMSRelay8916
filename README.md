# SMSRelay8916

jz02v10（MSM8916 / 高通 410）随身 WiFi 的短信网关。收短信、归档、钉钉转发、Web 收发。

Go 重写版，替换早期 Python 实现（`/opt/sms-gw/sms-gw.py`）。API 与页面完全兼容，共用同一个 SQLite 归档库。

> 早期 Python 版已从设备上移除，运行期只依赖这一个静态二进制；`/usr/local/bin/sms` 命令行工具改为 `curl + jq`，**python3 也已从设备卸载**（需要时 `apt-get install python3` 装回即可）。

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
internal/otp/otp.go           验证码提取（纯函数，上下文优先 + 克制的兜底）
internal/logbuf/logbuf.go     运行日志内存环形缓冲（Web 端查看，journal 仍负责持久化）
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
4. `CGO_ENABLED=1` + 系统 `libsqlite3`（`-tags libsqlite3`）

sqlite 驱动用 `mattn/go-sqlite3`，构建时链 OpenWrt 的 `libsqlite3`（feeds 里的
`libs/sqlite3`），而不是把 SQLite 编译进二进制：主程序小约 4.5MB，且与系统里其它
用 sqlite 的程序共用同一份库。代价是必须用 OpenWrt 工具链编译（宿主机 gcc 编不出
能在 musl 上跑的二进制），所以正式发版走 ipk，`openwrt/deploy.sh` 也要 `STAGING_DIR`。

## 部署

正式路径是 OpenWrt 包：

```bash
cp -a SMSRelay8916/openwrt package/smsrelay
./scripts/feeds update packages && ./scripts/feeds install golang libsqlite3
make package/smsrelay/compile V=99        # 产物在 bin/packages/*/base/
```

不重刷固件的快速迭代通道（需要构建树的 staging_dir）：

```bash
STAGING_DIR=$PWD/staging_dir ./openwrt/deploy.sh <设备IP> <密码>
```

脚本会：用工具链交叉编译（CGO）→ 检查设备有 `libsqlite3` → 上传二进制/init/UCI/LuCI →
启用并启动服务。UCI 配置只在设备上还不存在时推送，不会覆盖已改过的值。

## 配置

配置的唯一来源是 UCI `/etc/config/smsrelay`，用 **LuCI → 服务 → 短信网关 → 设置**
修改；`uci commit` 后 procd 按 reload trigger 自动重启服务。程序**只读**这个文件，
不回写，所以不存在两个写入口互相覆盖的问题。

```
config smsrelay 'main'
	option enabled '1'
	option listen_host '0.0.0.0'
	option listen_port '8088'
	option api_token ''
	option db_path '/etc/smsrelay/sms.db'
	option retain_days '30'
	option own_number ''
	option bands ''

config channel 'ch_ops'
	option name '运维群'
	option type 'dingtalk'
	option url 'https://oapi.dingtalk.com/robot/send?access_token=...'
	list body_line '{"text":"%s"}'

config ddns 'd_home'
	option name 'home.example.com'
	option type 'A'
	option api_token '...'
	option source 'iface:wwan0'
```

- `api_token` 为空表示不鉴权；非空时需 `X-Token` 头或 `?token=`。
  `scripts/sms` 用 `uci get` 自动读取，不用手工同步。
- 通道/DDNS 的**段名是稳定主键**（`ch_ops`、`d_home`），库里按它记投递历史。
  LuCI 生成的段名（`cfg0xxxx`）请勿手改，否则历史会对不上。
- 多行模板用 `list body_line`，每行一项；正文占位符是 `%s`。
- 请求头用 `list header 'K: V'`。
- `bands` 留空表示不动模组频段（默认）。本机模组默认只开 `eutran-1/3/5`，
  如果所在位置的移动小区在 band 8/39/40 上，就会出现「手机有信号、模组扫不到 46000」，
  表现为注册不上、一直 searching。这时把支持的频段全填上，保活脚本会在基带就绪时
  通过 `mmcli --set-current-bands` 设置一次（已在目标频段则跳过，不打断链路）。
  下发的值会先与「本机模组 supported」求交集，所以**同一份配置可以跨机型共用**：
  sp970 只支持 `egsm/dcs/pcs/g850/utran-1/5/eutran-1/3/5`，写全套也只会被裁到它支持的部分，
  不会因为「不支持该频段」失败；交集为空时打一条 WARN 并跳过。

  手写方式（也可在 LuCI「设置 → 模组频段」里填）：

  ```
  uci set smsrelay.main.bands='egsm|dcs|pcs|g850|utran-1|utran-2|utran-3|utran-5|utran-8|eutran-1|eutran-2|eutran-3|eutran-5|eutran-7|eutran-8|eutran-20|eutran-39|eutran-40'
  uci commit smsrelay
  ```

## 运行期组件

除了主程序，还有三个辅助件。它们的源码都在仓库里，`./deploy.sh` 会一起安装——
不需要手工往设备上拷文件，改完脚本重新部署即可生效。

| 组件 | 仓库文件 | 设备路径 | 作用 |
|---|---|---|---|
| 主程序 | `cmd/`（编译产物） | `/opt/smsrelay/smsrelay` | 收信 / 归档 / 转发 / HTTP |
| LTE 保活+自愈 | `openwrt/files/usr/bin/smsrelay-keepalive` | `/usr/bin/smsrelay-keepalive` | 每 10 秒一个 ICMP（按 wwan0 的源地址发，绑设备名会假失败）避免空闲态丢寻呼；连续 3 次不通走 `ifdown/ifup modem` 重拨（5 分钟冷却）；基带未就绪时静默等待、不计失败次数；按 `bands` 配置设置模组频段，全程不碰 AT 口 |
| 每日清理 | `scripts/smsrelay-maint.sh`<br>`deploy/smsrelay-maint.service`<br>`deploy/smsrelay-maint.timer` | `/usr/local/bin/smsrelay-maint.sh` | 每天 04:10 停 MM 独占 AT 口，清掉孤儿短信分片（纯 shell，不依赖 python3） |
| 命令行 | `scripts/sms` | `/usr/local/bin/sms` | 只依赖 `curl` + `jq` |

```bash
sms status              # 号码 / 信号 / 收发统计 / 投递延迟
sms list 20             # 最近 20 条
sms send 10086 cxye     # 发送（超长自动拆分）
sms fwd-test            # 向所有启用通道发测试消息
```

`deploy.sh` 装之前会先 `sh -n` 验语法——装一个跑不起来的清理脚本比不装更糟。
设备上缺 `jq` 时只告警不中断，`sms` 命令届时不可用（`apt-get install jq` 即可）。

数据只有一处：`/etc/smsrelay/sms.db`（overlay 持久；不要放 `/var`，那是 tmpfs）。

| 路径 | 内容 | 升级后 |
|---|---|---|
| `/etc/config/smsrelay` | **全部配置**（启动参数、通道、DDNS），声明为 conffile | **保留** |
| `/etc/smsrelay/sms.db` | 短信归档 + 投递记录 + DDNS 运行态 | 丢弃（可丢数据） |
| `/var/log` | 运行日志（tmpfs） | 不保留 |

### 升级（sysupgrade）的行为

配置在 `/etc/config/` 下，`base-files` 的保留清单本来就覆盖它，所以**配置自动保留，
不需要任何自定义 keep.d**。短信库不在保留清单里，升级时随 overlay 一起清空 ——
这是刻意的：历史短信不需要跨版本带走，库坏了也直接删掉重建即可，不会影响服务启动。

> 想要连短信一起带走的话，在 `/etc/sysupgrade.conf` 里加一行 `/etc/smsrelay/`，
> 但要注意 sysupgrade 打包配置时服务还在跑，库是被热拷的。

## 转发通道与模板

通道在 LuCI 里配置（`/etc/config/smsrelay` 的 `config channel` 段），支持 `dingtalk`
与 `webhook` 两种类型，可同时启用多条。每条通道的投递结果独立记账（
`channel_deliveries` 表），一条失败不会让其它通道重推。API 的 `/api/channels` 只读，
写入会返回 403 并提示去 LuCI 改。

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
| `/etc/config/smsrelay`（含 token、本机号码） | 设备本地配置，入库的是 `openwrt/files/` 下的**示例值**，不含真实凭证 |
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
| GET | `/api/messages?limit=30&offset=0&dir=in&q=关键词&has_code=1` | 分页查询；`has_code=1` 只看提取到验证码的 |
| POST | `/api/send` | `{"number":"10086","text":"cxye"}` |
| POST | `/api/forward-test` | 钉钉连通性测试 |
| POST | `/api/forward` | `{"id":N}` 重新入队 |
| DELETE | `/api/messages/{id}` | 删除归档记录 |
| POST | `/api/messages/batch-delete` | `{"ids":[1,2,3]}` 批量删除（≤1000），连带投递记录 |
| POST | `/api/maintenance/cleanup` | 按当前 `retain_days` 立即清一次过期记录 |
| GET | `/api/logs?limit=300&level=warn` | 内存环形缓冲里的运行日志（最多 500 条） |
| GET | `/api/wifi/status` / `/api/wifi/scan` / `/api/wifi/saved` | Wi-Fi 状态 / 周边扫描 / 已保存网络 |
| POST | `/api/wifi/connect` | `{ssid, password}` 连接（同步，最长 ~50s） |
| DELETE | `/api/wifi/saved/{name}` | 忘记网络；当前连接返回 409 |

### Wi-Fi 管理

设置页「Wi-Fi」区可扫描、连接（密码走界面输入）、忘记已保存网络。防自锁设计：
活跃连接的 profile 禁止删除；连接失败 NM 自动回连原网络；短信走 LTE 不受影响。
实现是 nmcli 封装（`internal/wifi`），固定 `LC_ALL=C` 解析，SSID/密码只走 argv。

### 转发规则路由

每个通道可设条件（全部为空 = 接收所有，向后兼容）：对端号码**前缀**、正文关键词、
排除关键词、仅验证码。非空条件 AND 组合，条件内逗号分隔任一命中。所有通道都不匹配 =
消息标记已转发（无事可做）。通道测试（fwd-test）始终旁路条件。

### PWA

页面带 manifest 与图标（AI 生成，512/192）。iOS Safari「添加到主屏幕」完整可用；
Android 在 HTTP 下只能创建快捷方式，上 HTTPS 后自动升级为完整 PWA（本轮不做 HTTPS）。

### 验证码提取

入站短信归档时自动提取验证码（`internal/otp`）：优先「验证码/校验码/code is」等关键词
上下文，兜底仅当正文含验证类字样时取独立 4-8 位数字（跳过 `138****1234` 式打码段，
11 位手机号因无词边界自然排除）。提取结果用于：列表高亮与一键复制、钉钉默认模板首行
`验证码：xxxx`（正文没有验证码时模板不变）、SSE 事件与 CLI `sms code`。自定义 webhook
模板可用 `{{code}}` / `{{code_json}}`。

### 归档保留策略

设置页「归档保留天数」（`retain_days`，0–3650，0 或留空 = 关闭）。开启后每 6 小时
删除超过天数的归档记录及其通道投递记录，删完截断 WAL（设备只有 3.3GB eMMC）。
设置页有「立即清理」按钮。`maint.sh` 保持 modem 侧单一职责，不参与。

### 日志查看

进程内 500 条环形缓冲（`internal/logbuf`）挂在 `log` 输出后，Web「日志」Tab 查看，
支持 WARN 过滤。重启即清空，完整历史仍在设备上 `journalctl -u smsrelay`。

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

`AT+CREG: 0,1` 显示 CS 域已注册，但空闲寻呼实际打不到，属固件/网络侧行为，软件改不动根因，只能缓解：`smsrelay-keepalive`（procd 的 keepalive 实例）每 10 秒 ping 一次，维持连接活跃（约 0.5 MB/天）。

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
