# SPDX-License-Identifier: MIT
#
# smsrelay —— jz02v10 (MSM8916) 短信网关
#
# 接入 OpenWrt/ImmortalWrt 构建树（在构建树根目录执行）：
#   cp -a <SMSRelay8916>/openwrt package/smsrelay        # 或 ln -s
#   ./scripts/feeds update packages && ./scripts/feeds install golang
#   make package/smsrelay/compile V=99
#
# 源码由构建树自己从 GitHub 拉取（本目录只放打包脚本和 files/ 覆盖层）。
# PKG_SOURCE_VERSION 固定 commit 保证可复现；CI 想跟随分支最新，
# 在 make 前 export PKG_SOURCE_VERSION=<commit> 即可覆盖。
# 更新版本时：改 PKG_SOURCE_VERSION 指向新 commit，PKG_RELEASE +1。

include $(TOPDIR)/rules.mk

PKG_NAME:=smsrelay
PKG_VERSION:=0.1.0
PKG_RELEASE:=3

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/zhoufla/SMSRelay8916.git
PKG_SOURCE_VERSION?=42c3ff41d06b7b0d117e1de2455ab2fd82a86796
PKG_MIRROR_HASH:=skip

PKG_MAINTAINER:=zhoufla <https://github.com/zhoufla>
PKG_BUILD_DEPENDS:=golang/host libsqlite3
PKG_BUILD_PARALLEL:=1

GO_PKG:=github.com/zhoufla/SMSRelay8916
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/smsrelay
GO_PKG_LDFLAGS_X:=main.Version=$(PKG_VERSION)
# 链系统的 libsqlite3，而不是让 mattn/go-sqlite3 内嵌一份 amalgamation：
# 主程序因此小约 4.5MB，且与系统里其它用 sqlite 的程序共用同一份库。
# golang-package.mk 默认 CGO_ENABLED=1，并会带上 TARGET_CC 与 CGO_*FLAGS。
GO_PKG_TAGS:=libsqlite3
# 去掉 DWARF 与符号表：Go 二进制带调试信息约 16MB，去掉后约 7.7MB。
# 设备上只有 192MB rootfs，这 8MB 值得省；排障靠日志而非 gdb。
GO_PKG_LDFLAGS:=-s -w
# go:embed 的 Web 资源（index.html / manifest / icons）不属于默认拷贝的
# 源码扩展名清单，不显式带上构建会直接失败。匹配的是相对路径。
GO_PKG_INSTALL_EXTRA:=^./internal/api/web/

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/smsrelay
  SECTION:=net
  CATEGORY:=Network
  TITLE:=SMS gateway for jz02v10 (MSM8916 stick)
  URL:=https://github.com/zhoufla/SMSRelay8916
  DEPENDS:=$(GO_ARCH_DEPENDS) +dbus +modemmanager +libsqlite3
endef

define Package/smsrelay/description
  SMS gateway for the jz02v10 (Zhihe MSM8916) 4G stick.
  Receives SMS via ModemManager D-Bus signals (zero polling when idle),
  archives to SQLite, forwards to DingTalk/webhook channels and serves
  a Web UI on :8088, embedded into LuCI as an iframe.
  All configuration lives in UCI (/etc/config/smsrelay) and is edited
  from LuCI; the daemon only reads it. The SQLite DB holds the SMS
  archive and runtime state (delivery results / DDNS last IP), which
  is disposable across firmware upgrades.
endef

define Package/smsrelay/conffiles
/etc/config/smsrelay
endef

define Package/smsrelay/install
	$(call GoPackage/Package/Install/Bin,$(1))

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/etc/init.d/smsrelay $(1)/etc/init.d/smsrelay
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/etc/config/smsrelay $(1)/etc/config/smsrelay

	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) ./files/usr/bin/smsrelay-keepalive $(1)/usr/bin/smsrelay-keepalive

	# LuCI 菜单 / ACL / 视图（iframe 嵌 :8088 自带页面）
	$(INSTALL_DIR) $(1)/usr/share/luci/menu.d
	$(INSTALL_DATA) ./files/usr/share/luci/menu.d/luci-app-smsrelay.json $(1)/usr/share/luci/menu.d/
	$(INSTALL_DIR) $(1)/usr/share/rpcd/acl.d
	$(INSTALL_DATA) ./files/usr/share/rpcd/acl.d/luci-app-smsrelay.json $(1)/usr/share/rpcd/acl.d/
	$(INSTALL_DIR) $(1)/www/luci-static/resources/view/smsrelay
	$(INSTALL_DATA) ./files/www/luci-static/resources/view/smsrelay/index.js \
		$(1)/www/luci-static/resources/view/smsrelay/index.js
	$(INSTALL_DATA) ./files/www/luci-static/resources/view/smsrelay/config.js \
		$(1)/www/luci-static/resources/view/smsrelay/config.js
endef

$(eval $(call GoBinPackage,smsrelay))
$(eval $(call BuildPackage,smsrelay))
