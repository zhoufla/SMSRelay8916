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
# 默认跟随 main；发布/CI 构建在本包 Makefile 中固定为具体 commit，
# 不导出全局 PKG_SOURCE_VERSION，以免污染构建树中的其他软件包。

include $(TOPDIR)/rules.mk

PKG_NAME:=smsrelay
PKG_VERSION:=0.2.0
PKG_RELEASE:=1

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/zhoufla/SMSRelay8916.git
PKG_SOURCE_VERSION?=main
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
  archives to SQLite and forwards to DingTalk/webhook channels.
  Local RPC uses a permission-restricted Unix socket, with no HTTP listener.
  Configuration is stored in UCI; SMS history survives configuration-preserving upgrades.
endef

define Package/smsrelay/conffiles
/etc/config/smsrelay
endef

define Package/luci-app-smsrelay
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=Native LuCI SMS gateway and MSM8916 controls
  DEPENDS:=+smsrelay +luci-base +rpcd +rpcd-mod-file
  PKGARCH:=all
endef

define Package/luci-app-smsrelay/conffiles
/etc/config/jz02
endef

define Package/smsrelay/install
	$(call GoPackage/Package/Install/Bin,$(1))

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/etc/init.d/smsrelay $(1)/etc/init.d/smsrelay
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/etc/config/smsrelay $(1)/etc/config/smsrelay

	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) ./files/usr/bin/smsrelay-keepalive $(1)/usr/bin/smsrelay-keepalive
	$(INSTALL_BIN) ./files/usr/bin/sms $(1)/usr/bin/sms
	$(INSTALL_DIR) $(1)/usr/libexec/rpcd
	$(INSTALL_BIN) ./files/usr/libexec/rpcd/smsrelay $(1)/usr/libexec/rpcd/smsrelay
	$(INSTALL_DIR) $(1)/lib/upgrade
	$(INSTALL_DATA) ./files/lib/upgrade/smsrelay.sh $(1)/lib/upgrade/smsrelay.sh
	$(INSTALL_DIR) $(1)/etc/uci-defaults
	$(INSTALL_BIN) ./files/etc/uci-defaults/90-smsrelay $(1)/etc/uci-defaults/90-smsrelay
endef

define Package/luci-app-smsrelay/install
	$(INSTALL_DIR) $(1)/etc/config $(1)/etc/init.d
	$(INSTALL_CONF) ./files/etc/config/jz02 $(1)/etc/config/jz02
	$(INSTALL_BIN) ./files/etc/init.d/jz02-power $(1)/etc/init.d/jz02-power
	$(INSTALL_BIN) ./files/etc/init.d/jz02-led $(1)/etc/init.d/jz02-led
	$(INSTALL_DIR) $(1)/usr/share/luci/menu.d
	$(INSTALL_DATA) ./files/usr/share/luci/menu.d/luci-app-smsrelay.json $(1)/usr/share/luci/menu.d/
	$(INSTALL_DIR) $(1)/usr/share/rpcd/acl.d
	$(INSTALL_DATA) ./files/usr/share/rpcd/acl.d/luci-app-smsrelay.json $(1)/usr/share/rpcd/acl.d/
	$(INSTALL_DIR) $(1)/www/luci-static/resources/view/smsrelay
	$(INSTALL_DATA) ./files/www/luci-static/resources/view/smsrelay/*.js $(1)/www/luci-static/resources/view/smsrelay/
	$(INSTALL_DATA) ./files/www/luci-static/resources/smsrelay.js $(1)/www/luci-static/resources/smsrelay.js
endef

$(eval $(call GoBinPackage,smsrelay))
$(eval $(call BuildPackage,smsrelay))
$(eval $(call BuildPackage,luci-app-smsrelay))
