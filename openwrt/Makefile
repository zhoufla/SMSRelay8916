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
PKG_RELEASE:=1

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/zhoufla/SMSRelay8916.git
PKG_SOURCE_VERSION?=9a2703ff4105cbd691032bf21aa811314ae94d8c
PKG_MIRROR_HASH:=skip

PKG_MAINTAINER:=zhoufla <https://github.com/zhoufla>
PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1

GO_PKG:=github.com/zhoufla/SMSRelay8916
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/smsrelay
GO_PKG_LDFLAGS_X:=main.Version=$(PKG_VERSION)
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
  DEPENDS:=$(GO_ARCH_DEPENDS) +dbus +modemmanager
endef

define Package/smsrelay/description
  SMS gateway for the jz02v10 (Zhihe MSM8916) 4G stick.
  Receives SMS via ModemManager D-Bus signals (zero polling when idle),
  archives to SQLite, forwards to DingTalk/webhook channels and serves
  a Web UI on :8088, embedded into LuCI as an iframe.
  Startup config comes from UCI (/etc/config/smsrelay); runtime state
  (channels / DDNS / settings) lives in the app's own SQLite DB.
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
endef

$(eval $(call GoBinPackage,smsrelay))
$(eval $(call BuildPackage,smsrelay))
