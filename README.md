# SMSRelay8916

MSM8916（jz02v10 / sp970v11）OpenWrt 短信网关：原生 LuCI、SQLite 收发记录、验证码提取、多通道转发、DDNS 和 LTE 保活。

- `main`：OpenWrt 主线。
- `debian`：原 Debian 版本备份；systemd、独立网页及 HTTP API 的用法请查看该分支。
- `openwrt`：旧兼容分支，不作为新版本发布入口。

## 界面

进入 **LuCI → 服务 → 短信网关**，四个页签：

| 页签 | 内容 |
|---|---|
| 设置 | 基本参数、归档保留策略、转发通道、DDNS、LTE，以及原 `/system/jz02` 的调频和 LED 设置 |
| 状态 | 逐核频率、CPU/基带温度、调频范围、模组/网络状态、短信统计和 DDNS 状态 |
| 短信记录 | 收件箱、发件箱、搜索、分页、验证码复制、详情、删除、重试转发、手动发送 |
| 日志 | 最近 500 条内存日志，级别/关键词筛选、暂停自动刷新、复制和下载 |

状态页没有对应温度传感器时显示“暂无传感器”，不会使用 PMIC 温度代替 CPU 或基带温度。
系统数据只在页面打开时采集；短信后台不再定时采样 CPU、温度或系统流量。

## 架构

```text
原生 LuCI JavaScript
      │ rpcd / ubus（读写 ACL 分离）
      │ /usr/libexec/rpcd/smsrelay（按需执行，不常驻）
      │ /usr/bin/smsrelay rpc call <固定白名单方法>
      │ Unix Socket，目录 0700 / Socket 0600
smsrelay（procd 管理）
      ├─ ModemManager D-Bus 信号 + mmcli JSON：接收/发送
      ├─ SQLite WAL：短信、发送队列、通道投递结果、DDNS 运行态
      └─ 转发、保留策略、DDNS
```

**没有 TCP/HTTP 监听、8088 端口、iframe、独立网页或 PWA。**
LuCI 的登录和 rpcd ACL 是唯一网页鉴权入口，旧 `listen_host/listen_port/api_token/web_dir` 配置已废弃。
Cloudflare 记录自己的 `api_token` 不受影响。

LTE 保活脚本继续作为 procd 的独立实例运行；它不直接访问 AT 口。
JZ02 性能和指示灯脚本由 `luci-app-smsrelay` 包安装，不再由云编译仓库复制覆盖。

## 构建与安装

需要带 Go 1.26 工具链的 OpenWrt/ImmortalWrt 构建树或 SDK。
SQLite 通过 CGO 链接目标系统 `libsqlite3`；不能用 `CGO_ENABLED=0` 或宿主机 gcc 生成可用的 OpenWrt 版本。

在构建树根目录：

```sh
cp -a /path/to/SMSRelay8916/openwrt package/smsrelay
./scripts/feeds update packages
./scripts/feeds install golang libsqlite3
# 在 package/smsrelay/Makefile 中将 PKG_SOURCE_VERSION 固定为本次已发布的提交。
# 未提交代码可使用 OpenWrt 的本地源码覆盖机制测试，不能依赖远端下载。
make menuconfig  # 同时选择 smsrelay 与 luci-app-smsrelay
make package/smsrelay/compile V=99
```

同一个包目录生成两个软件包：

- `smsrelay`：Go 后台、UCI、LTE 保活、本地 RPC 桥接和升级快照逻辑。
- `luci-app-smsrelay`：四页签、权限、菜单、JZ02 配置及性能/LED 脚本，依赖 `smsrelay`。

云编译项目 [MSM8916-OpenWrt-CI](https://github.com/zhoufla/MSM8916-OpenWrt-CI) 默认选择两个包，并将拉取到的 `main` 提交写入包 Makefile，保证包定义与 Go 源码一致。
两个仓库的配套改动应一起发布后再构建固件。

不重刷固件的开发部署：

```sh
STAGING_DIR=/path/to/openwrt/staging_dir \
  SMSRELAY_SSH_PASS='<设备密码>' ./openwrt/deploy.sh <设备IP>
```

只在设备配置不存在时上传示例 UCI，不覆盖已有通道或凭据。
正式发布建议安装软件包，而不是手工部署。

## 配置与数据

| 路径 | 用途 |
|---|---|
| `/etc/config/smsrelay` | 全部短信、转发、DDNS、保活配置 |
| `/etc/config/jz02` | CPU 调频、频率上限、Wi-Fi 活动灯 |
| `/etc/smsrelay/sms.db` | 持久短信归档、发送任务、通道投递结果、DDNS 状态 |
| `/var/run/smsrelay/control.sock` | 仅本机可访问的 RPC Socket |

配置只通过 UCI 修改，后台只读加载。短信配置提交后重启短信服务；JZ02 配置通过自己的 reload trigger 应用，不重启短信服务。
没有硬件支持时隐藏对应设置。频段、运营商锁定留空时维持模组默认行为。

短信保留天数默认 30，`0` 表示不自动清理；周期为 6 小时，设置页也可手动清理。
排队中和发送中的记录不允许删除。删除短信会清理相关发送任务及投递数据，不留下孤立的正文副本。
修改 `db_path` 不会自动搬迁原库；应先停服务，再迁移数据库及 WAL/SHM，或使用一致性备份。
数据库及 Socket 均限制权限，备份压缩包包含私人短信，须妥善保管。

### 升级与备份

保留配置的 `sysupgrade` / `sysupgrade -b` 会调用 `/lib/upgrade/smsrelay.sh`：

1. 真正刷写时先停短信服务，再通过 SQLite `VACUUM INTO` 生成包含已提交 WAL 数据的一致性快照；单独 `sysupgrade -b` 则在线备份调用瞬间的数据。
2. 从备份清单排除活动 `.db/-wal/-shm`，只打包 `/etc/smsrelay/upgrade.db`。
3. 新固件首次启动时校验快照；目标库不存在时恢复，并消费快照。
4. 已存在的数据库（例如外接持久盘上的库或软件包重装）不会被旧快照覆盖。

快照失败会阻止备份，不静默忽略。`sysupgrade -n`、清空 overlay、fastboot 直接重刷不保证保留数据。
外接存储必须在恢复与服务启动前挂载；重要短信建议另做离机备份。
数据库损坏时不会自动删库或更换为空库，保留原文件和模组中的短信，并在系统日志报告错误。

从旧固件首次升级前，旧系统尚没有快照钩子，必须先离线备份：停短信服务，再完整备份数据库及可能存在的 WAL/SHM；不要只热拷 `.db`。

### 可靠收发

- 模组 ME/SIM 槽位有限，收到短信必须归档后释放，**只有确认入库才删除模组副本**。
- 使用 `pdu 类型 + 号码 + 时间戳 + 正文` 去重，不单用可重复分配的 SMS 对象路径。
- 长短信正文须稳定 6 秒才归档，不与 ModemManager 争用 AT 端口。
- 手动发送先将请求及分段原子写入 SQLite，再由后台串行发送。
- 同一个请求标识的重复提交返回原任务，不重复发送。
- 未确认的发送结果显示“结果待确认”，不把超时当成功，不盲目自动重发；重启时同样保守处理。
- 失败后重新编辑并确认发送；运营商可能收费，结果待确认时应先核实。

### 转发模板

Webhook / 钉钉支持多个通道，投递结果独立记录；重试跳过已经成功的通道。
号码前缀、正文关键词、排除关键词、仅验证码等条件之间为“且”。
UCI 段名是稳定主键，不要手动改名；浏览器操作使用段名，避免 int64 主键在 JavaScript 中丢失精度。

模板支持 `{{own_number}}`、`{{number}}`、`{{body}}`、`{{code}}`、`{{ts}}`、`{{dir}}`、`{{id}}`、`{{prefix}}`。
JSON 模板使用对应的 `_json` 占位符，它自带引号，例如：

```json
{"from":{{number_json}},"text":{{body_json}},"code":{{code_json}}}
```

## 本地 RPC 与日志

设备上可使用 ubus；调用返回 `{ "ok": true, "data": ... }` 或结构化错误：

```sh
ubus call smsrelay status '{}'
ubus call smsrelay messages '{"dir":"in","limit":30,"offset":0}'
ubus call smsrelay logs '{"limit":100,"level":"warn"}'
```

日志缓冲最多 500 条，进程重启即清空，支持 INFO/WARN/ERROR。
配置中的密钥、URL 和长号码会在输出前脱敏；不记录短信正文。
标准错误仍进入 OpenWrt `logd`，可通过 `logread -e smsrelay` 排查启动失败；是否跨重启保存由系统日志配置决定。

## 开发验证

```sh
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

安装 Playwright 到独立测试目录后，可运行浏览器冒烟测试（不运行前端构建）：

```sh
PLAYWRIGHT_MODULE=/path/to/playwright node scripts/test-luci.cjs
```

测试使用真实页面模块，但 RPC、sysfs、LuCI 表单和主题为替身；默认截图位于 `/tmp/opencode/smsrelay-status-preview.png`，不代表真机效果。

覆盖原有 OTP/转发/存储测试以及本地 RPC、发送请求幂等、恢复策略和 WAL 在线快照。
LuCI/rpcd 权限、跨架构打包、实际收发、温度探测及 sysupgrade 完整流程仍需在对应 OpenWrt 设备验证。
