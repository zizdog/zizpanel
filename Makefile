# =============================================================================
#  ZizPanel 构建入口
#
#  常用目标：
#    make dev        本地构建（快速，用于开发）
#    make test       跑全部单元测试
#    make check      格式检查 + vet + 测试（提交前跑这个）
#    make uitest     端到端 UI 验证（需要先 make run-local）
#    make run-local  在临时目录以调试模式启动面板
#    make release    产出可分发压缩包到 dist/release/
#    make install    本机安装（等价于 sudo bash install.sh）
#    make clean      清理构建产物
#
#  为什么用 Makefile 而不是一堆散落的 .sh：
#   每个动作有唯一入口、有依赖关系、可重复执行。发布流程尤其重要 ——
#   少打一个架构或漏了版本号，用户就会下载到一个装不上的包。
# =============================================================================

SHELL      := /bin/bash
# VERSION 必须**锚定 `var Version`**，不能用"文件里第一个 x.y.z"：
# version.go 的文档注释里会写历史版本（如 `1.0.0：第一个正式版`），
# 宽松 grep + head -1 会取到注释里的旧版本 → 包名/清单用旧版本、二进制却是新的，
# 而 `make release` 不会报任何错（2026-09-17 实测踩到，见 坑清单 坑 153）。
VERSION    := $(shell sed -n 's/^var Version *= *"\([0-9][0-9.]*\)".*/\1/p' internal/version/version.go | head -1)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
	-X github.com/zizdog/zizpanel/internal/version.Version=$(VERSION) \
	-X github.com/zizdog/zizpanel/internal/version.Commit=$(COMMIT) \
	-X github.com/zizdog/zizpanel/internal/version.BuildTime=$(BUILD_TIME)

# 国内网络下 proxy.golang.org 常不可达，默认走 goproxy.cn
export GOPROXY ?= https://goproxy.cn,direct
export GOFLAGS ?= -mod=mod
CGO_ENABLED ?= 0

DIST    := dist
RELDIR  := $(DIST)/release
# ARCHS 是要打入发布包的架构。默认双架构（正式发布别漏 amd64）；
# `make deploy` 会传 ARCHS=arm64 —— 本机与 mini 都是 Apple Silicon，
# 另一份包纯属浪费（构建时间 + 上传体积翻倍）。
ARCHS   ?= arm64 amd64
LOCAL_PORT ?= 18443
LOCAL_ROOT ?= /tmp/zizpanel-dev
# 本地调试实例的安全后缀：固定值，方便 uitest 直接访问。
# 真机安装时由 config.Bootstrap 随机生成（见 internal/config）。
LOCAL_SUFFIX ?= dev
SHOTS   ?= /tmp/zizpanel-shots

.PHONY: help
help: ## 显示所有可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- 开发构建 --
.PHONY: dev
dev: ## 本地构建（当前架构）
	@mkdir -p $(DIST)
	@# 如果本机存在发布私钥，就把对应公钥一起注入：
	@# 这样本地/真机装出来的面板也能验证网络升级包，便于完整演练升级流程。
	@# 只是公钥，注入它没有任何泄密风险。
	@PUB=""; \
	if [ -f "$(RELEASE_KEY)" ]; then \
		go build -o $(DIST)/keysgen ./cmd/zizpanel 2>/dev/null && \
		PUB="$$($(DIST)/keysgen sign-manifest --pub-from-key $(RELEASE_KEY) 2>/dev/null || true)"; \
	fi; \
	go build -ldflags "$(LDFLAGS) -X github.com/zizdog/zizpanel/internal/upgrade.PubKeyHex=$$PUB" -o $(DIST)/zizpanel ./cmd/zizpanel; \
	go build -ldflags "$(LDFLAGS)" -o $(DIST)/zizpanel-helper ./cmd/zizpanel-helper; \
	if [ -n "$$PUB" ]; then echo "已注入发布公钥 $$(printf '%s' "$$PUB" | cut -c1-16)…"; \
	else echo "未注入发布公钥（没有 $(RELEASE_KEY)）：网络升级会被拒绝，仅手动上传可用"; fi
	@echo "构建完成：$(DIST)/zizpanel ($(VERSION)+$(COMMIT))"

.PHONY: fmt
fmt: ## 格式化代码
	gofmt -w .

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: test
test: ## 运行全部单元测试（含"不污染用户真实家目录"门禁）
	@bash tools/check-test-pollution.sh

.PHONY: jobs-test
jobs-test: ## receiver.py 的 /jobs 验收（本地假上游，不需要 GPU）
	@echo "==> receiver /jobs 单元测试"
	@python3 tools/test-voice-jobs-unit.py
	@echo "==> receiver /jobs 端到端测试（假上游）"
	@python3 tools/test-voice-jobs.py

.PHONY: test-short
test-short: ## 只跑单测（跳过真实系统采集）
	go test ./... -short -count=1

.PHONY: check
check: ## 提交前检查：格式 + shell 校验 + vet + 测试
	@echo "==> 版本号来源检查（注释里的历史版本不许遮蔽 var Version；install.sh 的 SCRIPT_VERSION 必须一致）"
	@real="$(VERSION)"; loose=$$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' internal/version/version.go | head -1); \
	 scriptv=$$(sed -n 's/^SCRIPT_VERSION="\([0-9][0-9.]*\)".*/\1/p' install.sh | head -1); \
	 if [ -z "$$real" ]; then echo "!! 从 var Version 取不到版本号"; exit 1; fi; \
	 if [ "$$loose" != "$$real" ]; then \
	   echo "!! version.go 里'第一个版本号'($$loose) 与 var Version ($$real) 不一致："; \
	   echo "   注释里的 x.y.z 会骗过按行取版本的脚本/工具（包名与二进制会错版本）。"; \
	   exit 1; \
	 fi; \
	 if [ "$$scriptv" != "$$real" ]; then \
	   echo "!! install.sh 的 SCRIPT_VERSION ($$scriptv) 与面板版本 ($$real) 不一致："; \
	   echo "   安装横幅会显示假版本号。执行 make bump（它现在会同时改两处）。"; \
	   exit 1; \
	 fi; echo "   ok：版本号 $$real（version.go 与 install.sh 一致）"
	@echo "==> gofmt 检查"
	@unformatted=$$(gofmt -l . | grep -v '^$$' || true); \
	 if [ -n "$$unformatted" ]; then echo "以下文件需要 gofmt："; echo "$$unformatted"; exit 1; fi
	@echo "==> shell 语法检查"
	@bash -n install.sh && bash -n uninstall.sh && bash -n tools/sandbox-install-test.sh && bash -n tools/takeover-panel-entry.sh && bash -n tools/server-mode.sh && bash -n tools/server-mode-test.sh && bash -n tools/serve-for-install.sh && bash -n tools/install-from-remote.sh && bash -n tools/remote-install-test.sh && bash -n tools/upgrade-e2e.sh && bash -n tools/sync-nas-apps.sh && bash -n tools/check-no-real-credentials.sh && bash -n tools/publish-release.sh && bash -n tools/seed-nas-brew.sh && echo "shell 语法 OK"
	@echo "==> shell 变量引用检查（防多字节变量名 bug）"
	@python3 tools/check-shell-vars.py install.sh uninstall.sh tools/sandbox-install-test.sh tools/takeover-panel-entry.sh tools/server-mode.sh tools/server-mode-test.sh tools/serve-for-install.sh tools/install-from-remote.sh tools/remote-install-test.sh tools/upgrade-e2e.sh tools/check-test-pollution.sh tools/sync-nas-apps.sh tools/publish-release.sh tools/seed-nas-brew.sh
	@echo "==> shellcheck"
	@if command -v shellcheck >/dev/null 2>&1; then \
	   shellcheck -S warning -e SC1091 install.sh uninstall.sh tools/sandbox-install-test.sh tools/takeover-panel-entry.sh tools/server-mode.sh tools/server-mode-test.sh tools/serve-for-install.sh tools/install-from-remote.sh tools/remote-install-test.sh tools/upgrade-e2e.sh tools/check-test-pollution.sh tools/sync-nas-apps.sh tools/check-no-real-credentials.sh tools/publish-release.sh tools/seed-nas-brew.sh || exit 1; \
	 else echo "（未安装 shellcheck，跳过：brew install shellcheck）"; fi
	@# 真实口令不许进仓库 —— 这条坑复发过两次（v0.3.1 基线 + 第九轮新增文件），
	@# 所以做成门禁而不是靠人记。没有凭据文件时它明确打印"跳过"，不假装通过。
	@echo "==> 真实凭据泄漏检查"
	@bash tools/check-no-real-credentials.sh
	@echo "==> 未来日期检查（注释/文档里不许有比今天更晚的日期）" && bash tools/check-future-dates.sh
	@echo "==> Python 工具语法检查"
	@python3 -m py_compile tools/make-manifest.py && echo "python 语法 OK"
	@# 前端语法必须用真正的 ES 解析器校验：`node --check` 对"对象字面量少一个 }"
	@# 这类错误返回 0，而浏览器直接拒绝执行 → 整个面板白屏、连行号都不给。
	@# 这个坑真踩过（docker-compose.js / docker-services.js），所以设成门禁。
	@echo "==> 前端 JS 语法检查（acorn）"
	@# assets/nav 是「导航页」的独立别名页（GET /nav/）：它同样会被浏览器
	@# 当模块解析，语法错误一样是白屏，所以一并纳入门禁。
	@if [ -d node_modules/acorn ]; then \
	   node tools/check-js-syntax.mjs internal/web/assets/js internal/web/assets/nav || exit 1; \
	 else echo "（未安装 acorn，跳过：npm install）"; fi
	@echo "==> go vet"
	@$(MAKE) --no-print-directory vet
	@echo "==> 单元测试"
	@$(MAKE) --no-print-directory test
	@# receiver.py 的 /jobs 是纯 Python，go test 覆盖不到。
	@# 它又是「关掉网站也能跑完」的唯一保障，所以进 check 门禁。
	@echo "==> receiver /jobs 测试"
	@$(MAKE) --no-print-directory jobs-test
	@echo "==> 安装脚本端到端测试（沙箱）"
	@$(MAKE) --no-print-directory install-test
	@echo "==> 远程一键安装测试（本地 HTTP 服务 + 沙箱）"
	@$(MAKE) --no-print-directory remote-test
	@echo "==> 服务器模式测试（SSH/电源/更新策略）"
	@$(MAKE) --no-print-directory server-mode-test
	@bash tools/check-stamp.sh write
	@echo "全部检查通过 ✅"

.PHONY: install-test
install-test: ## 安装脚本端到端测试（沙箱，无需 root）
	@bash tools/sandbox-install-test.sh

.PHONY: remote-test
remote-test: ## 远程一键安装测试：构建 → 本地 HTTP 共享 → 下载 → 沙箱安装
	@bash tools/remote-install-test.sh

.PHONY: server-mode-test
server-mode-test: ## 服务器模式测试（SSH 开启路径、pmset 能力探测、dry-run 安全性）
	@bash tools/server-mode-test.sh

.PHONY: serve-install
serve-install: ## 把本机变成安装源，供另一台 Mac 用一条 curl 命令安装
	@bash tools/serve-for-install.sh

.PHONY: ui-test
ui-test: smoke ## 别名：启动本地实例并做 UI 验证

# ------------------------------------------------------------- 本地试运行 --
.PHONY: run-local
run-local: dev ## 在临时目录以调试模式启动（端口 $(LOCAL_PORT)）
	@pkill -f 'zizpanel serve --config $(LOCAL_ROOT)' 2>/dev/null || true
	@rm -rf $(LOCAL_ROOT)
	@mkdir -p $(LOCAL_ROOT)/bin
	@# 把助手也放进本地根目录：面板的提权调用按 <root>/bin/zizpanel-helper 解析路径，
	@# 缺了它所有特权操作（建站、nginx 校验）都会报"提权助手不存在"，
	@# 报错信息会和真实故障混淆，排查起来很费时间。
	@cp -f $(DIST)/zizpanel-helper $(LOCAL_ROOT)/bin/zizpanel-helper
	@( $(DIST)/zizpanel serve --config $(LOCAL_ROOT)/data/config.json \
	     --listen 127.0.0.1:$(LOCAL_PORT) --no-tls --log-level debug \
	     --panel-suffix "$(LOCAL_SUFFIX)" \
	     > $(LOCAL_ROOT)/serve.log 2>&1 & )
	@sleep 2
	@echo "面板已启动：http://127.0.0.1:$(LOCAL_PORT)/$(LOCAL_SUFFIX)/"
	@cat $(LOCAL_ROOT)/serve.log

.PHONY: stop-local
stop-local: ## 停止本地试运行实例
	@pkill -f 'zizpanel serve --config $(LOCAL_ROOT)' 2>/dev/null && echo "已停止" || echo "没有运行中的实例"

.PHONY: uitest
uitest: ## 端到端 UI 验证（需要先 make run-local；特权步骤会显示为"跳过"）
	ZP_SKIP_PRIV=1 node tools/uitest.mjs http://127.0.0.1:$(LOCAL_PORT)/$(LOCAL_SUFFIX)/ $(SHOTS)

# 真实面板口令放本机、**不进仓库**（.panel-credential.local 已在 .gitignore 里）。
# 内容就一行：ZP_PASS=你的面板口令
CRED_FILE ?= .panel-credential.local

.PHONY: uitest-live
uitest-live: ## 对本机真实安装实例跑完整 UI 验证（含建站等特权步骤，需要已安装）
	@curl -fsSk --max-time 5 https://127.0.0.1:8443/api/v1/health >/dev/null \
	  || { echo "本机 8443 没有运行中的面板，请先 sudo bash install.sh"; exit 1; }
	@test -f $(CRED_FILE) || { \
	  echo "缺少 $(CRED_FILE) —— 它存真实面板口令，不进仓库。"; \
	  echo "创建方式："; \
	  echo "    printf 'ZP_PASS=你的面板口令\\n' > $(CRED_FILE) && chmod 600 $(CRED_FILE)"; \
	  exit 1; }
	@echo "注意：会在这台真实面板上创建再删除测试站点 $(ZP_TEST_SITE)"
	@# 真实实例带安全后缀，入口不是根路径 —— 从配置里读出来，别写死
	@SUFFIX=$$(python3 -c "import json;print(json.load(open('/opt/zizpanel/data/config.json')).get('panel_suffix',''))" 2>/dev/null); \
	 BASE="https://127.0.0.1:8443/$$SUFFIX/"; \
	 set -a; . ./$(CRED_FILE); set +a; node tools/uitest.mjs "$$BASE" $(SHOTS)

.PHONY: smoke
smoke: run-local ## 启动本地实例并做 UI 验证（特权步骤跳过，用 uitest-live 补全）
	@# ⚠️ 必须**无论成败都 stop-local**：以前写成 `smoke: run-local uitest`，
	# uitest 一失败 make 就中断，调试实例被留在用户机器上（真机发生过，
	# 遗留进程监听 127.0.0.1:18443 好几轮才被发现）。
	@# ⚠️ **必须带 ZP_SKIP_PRIV=1**：本地实例不是 root，建站/nginx 校验这类要动
	# 系统配置的步骤在这里必然失败（实测建站返回 500「sudo: a password is required」）。
	# uitest 会给这些步骤打印"跳过"并把清单汇总出来 —— 那是**如实跳过**，不是假装通过；
	# 完整的特权链路请对真实安装实例跑 `make uitest-live`。
	@ZP_SKIP_PRIV=1 node tools/uitest.mjs http://127.0.0.1:$(LOCAL_PORT)/$(LOCAL_SUFFIX)/ $(SHOTS); rc=$$?; $(MAKE) --no-print-directory stop-local >/dev/null 2>&1 || true; exit $$rc

# ---------------------------------------------------------------- 发布打包 --
# install.sh 在目标机上会用到这些脚本（入口接管 / 服务器模式 / 自检工具）。
# 少打一个不会报错，只会**静默降级**——用户装完找不到面板入口，非常难排查。
# 所以集中声明在这里，并由 make remote-test 断言压缩包里确实存在。
RUNTIME_TOOLS := tools/takeover-panel-entry.sh tools/panel-entry.awk \
                 tools/check-shell-vars.py tools/server-mode.sh \
                 tools/system-services.sh

# 发布签名密钥。私钥只在本机存在（.release-key/ 已 gitignore），
# 绝不能进仓库、更不能打进发布包 —— 否则任何人都能签出"合法"升级包。
RELEASE_KEY ?= .release-key/zizpanel-ed25519.key
# 代码签名证书（公钥部分随包分发，目标机由 install.sh 装进系统钥匙串并设信任）。
# 私钥在 .release-key/codesign/（已 gitignore，只存在于构建机）。
CODESIGN_CERT ?= .release-key/codesign/zp-codesign.crt
# 发布包对外可下载的地址前缀（写进 manifest.json 的资源 URL）
RELEASE_BASE_URL ?= https://github.com/zizdog/zizpanel/releases/download/$(VERSION)
# 可选：把更新说明写进这个文件，会被放进清单里展示给用户
NOTES_FILE ?= RELEASE_NOTES.md
# 自建国内镜像（`make publish-mirror` 用）。面板里的"升级源"也填这个地址。
MIRROR_BASE_URL ?= https://zizdog.com/zizpanel
# 公网镜像地址：**唯一真源是 internal/upgrade/source.go 的 MirrorSource**，
# 这里从代码里取，避免出现第二份（2026-09-20 用户要求：发布产物只允许公网地址）。
PUBLIC_MIRROR_URL ?= $(shell sed -n 's/.*MirrorSource = "\([^"]*\)".*/\1/p' internal/upgrade/source.go | head -1)
# make deploy 让本机面板走哪个升级源（默认公网镜像；你自己的镜像机可覆盖）。
MIRROR_URL ?= $(PUBLIC_MIRROR_URL)
# 镜像机（你自己的自建镜像）：地址/账号/目录**全部由调用者提供**，仓库里不留默认值。
# 口令不写进仓库（铁律 7）。发布时用 make publish-nas NAS_PASS='...' 传入。
NAS_HOST       ?=
NAS_USER       ?=
NAS_ROOT       ?=
# NAS_APPS_ROOT 是**应用安装包**镜像目录（与面板镜像同级）：
#   <NAS_APPS_ROOT>/<应用>/<版本>/<文件名> —— 见 tools/sync-nas-apps.sh
NAS_APPS_ROOT  ?=
NAS_PASS       ?=
# APPS_MIRROR_URL 是应用包镜像的对外基址（= 面板设置里的"镜像基址"）。
# sync-apps 上传后用它验收；面板也按这个基址取包。
APPS_MIRROR_URL ?= https://mirror.zizdog.com:8888
# 传给 sync-apps 的额外参数，例如 SYNC_ARGS='--dry-run' 或 SYNC_ARGS='--app frpc'
SYNC_ARGS      ?=

.PHONY: upgrade-e2e
upgrade-e2e: ## 在线升级真实演练（需要本机已安装面板；会真的升级并重启面板）
	@bash tools/upgrade-e2e.sh

.PHONY: keys
keys: ## 生成发布用 Ed25519 密钥对（只做一次；私钥务必离线备份）
	@mkdir -p $(DIST)
	@go build -o $(DIST)/keysgen ./cmd/zizpanel
	@$(DIST)/keysgen sign-manifest --gen-key $(RELEASE_KEY)
	@echo ""
	@echo "说明：make release 会自动把对应公钥注入发布二进制，无需手工填写。"
	@echo "     私钥丢失后，已安装的面板将无法再接受你签出的升级包。"
	@echo "     请立即把 $(RELEASE_KEY) 备份到离线介质。"

.PHONY: release
release: clean ## 产出可分发压缩包 + 签名清单（默认双架构；ARCHS="arm64" 只发本机架构）
	@# ARCHS：`make deploy` 只发 arm64（本机与 mini 都是 Apple Silicon）—— 少构建一次、
	@# 上传体积减半；手动 `make release` 做正式发布时保持默认双架构，别漏 amd64。
	@echo "==> 目标架构：$(ARCHS)"
	@mkdir -p $(RELDIR)
	@# 先构建一个本机版本：它既用来推导公钥，也用来给清单签名。
	@# 这样"签名私钥"与"面板内嵌公钥"必然配对 —— 靠人工填公钥迟早会不一致。
	@go build -trimpath -o $(DIST)/host-zizpanel ./cmd/zizpanel
	@set -e; \
	PUB=""; \
	if [ -f "$(RELEASE_KEY)" ]; then \
		PUB="$$($(DIST)/host-zizpanel sign-manifest --pub-from-key $(RELEASE_KEY))"; \
		echo "==> 使用发布密钥签名，公钥: $$PUB"; \
	else \
		echo "==> 警告：未找到 $(RELEASE_KEY)，本次发布【不签名】"; \
		echo "    已安装的面板会拒绝从网络升级（可改用手动上传升级包）。"; \
		echo "    执行 make keys 生成发布密钥。"; \
	fi; \
	for arch in $(ARCHS); do \
		echo "==> 构建 darwin/$$arch"; \
		GOOS=darwin GOARCH=$$arch go build -trimpath \
			-ldflags "$(LDFLAGS) -X github.com/zizdog/zizpanel/internal/upgrade.PubKeyHex=$$PUB" \
			-o $(DIST)/tmp-$$arch/zizpanel ./cmd/zizpanel; \
		GOOS=darwin GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/tmp-$$arch/zizpanel-helper ./cmd/zizpanel-helper; \
		if [ "$${SKIP_CODESIGN:-0}" = "1" ]; then \
			echo "    !! 跳过签名（SKIP_CODESIGN=1）：本次产物是 adhoc 签名，"; \
			echo "       用户升级后系统会要求重新授权（见 docs/坑清单.md #183）"; \
		else \
			bash tools/codesign-release.sh $(DIST)/tmp-$$arch/zizpanel com.zizpanel.panel; \
			bash tools/codesign-release.sh $(DIST)/tmp-$$arch/zizpanel-helper com.zizpanel.helper; \
		fi; \
		install -m 0755 install.sh $(DIST)/tmp-$$arch/install.sh; \
		if [ -f $(CODESIGN_CERT) ]; then \
			install -m 0644 $(CODESIGN_CERT) $(DIST)/tmp-$$arch/zizpanel-codesign.crt; \
		else \
			echo "    !! 没有 $(CODESIGN_CERT)：目标机不会自动信任签名证书，"; \
			echo "       用户升级后可能被系统要求重新授权（只影响授权持久性，不影响安装）"; \
		fi; \
		mkdir -p $(DIST)/tmp-$$arch/tools; \
		for t in $(RUNTIME_TOOLS); do \
			install -m 0644 "$$t" "$(DIST)/tmp-$$arch/tools/$${t#tools/}"; \
		done; \
		chmod 0755 $(DIST)/tmp-$$arch/tools/takeover-panel-entry.sh \
		           $(DIST)/tmp-$$arch/tools/server-mode.sh \
		           $(DIST)/tmp-$$arch/tools/system-services.sh; \
		if [ -n "$$PUB" ]; then \
			got="$$($(DIST)/tmp-$$arch/zizpanel version --json 2>/dev/null | sed -n 's/.*"upgrade_pubkey":"\([^"]*\)".*/\1/p')"; \
			if [ "$$got" != "$$PUB" ] && grep -q "$$PUB" $(DIST)/tmp-$$arch/zizpanel; then \
				got="$$PUB"; \
				echo "    （darwin/$$arch 无法在本机执行，已改用静态检查）"; \
			fi; \
			if [ "$$got" != "$$PUB" ]; then \
				echo "!! darwin/$$arch 没有正确内嵌发布公钥（-X 会静默失效！）"; \
				echo "   期望: $$PUB"; \
				echo "   实际: $$got"; \
				echo "   若实际为空，通常是该符号被死代码消除了 ——"; \
				echo "   确认 main 直接引用了 upgrade.PublicKeyHex()。"; \
				exit 1; \
			fi; \
			echo "    ✓ 已内嵌发布公钥 $$(printf '%s' "$$PUB" | cut -c1-16)…"; \
		fi; \
		( cd $(DIST)/tmp-$$arch && tar -czf ../release/zizpanel_$(VERSION)_darwin_$$arch.tar.gz . ); \
		rm -rf $(DIST)/tmp-$$arch; \
	done
	@# 同时产出一份"通用"名字的最新包，便于固定 URL 下载
	@for a in $(ARCHS); do cp $(RELDIR)/zizpanel_$(VERSION)_darwin_$$a.tar.gz $(RELDIR)/zizpanel_latest_darwin_$$a.tar.gz 2>/dev/null || true; done
	@# 生成清单：面板"检查更新"读的就是它。清单里带每个架构的 URL 与 SHA-256。
	@python3 tools/make-manifest.py --version $(VERSION) --dir $(RELDIR) \
		--arches "$(ARCHS)" --base-url "$(RELEASE_BASE_URL)" --notes-file "$(NOTES_FILE)"
	@set -e; \
	if [ -f "$(RELEASE_KEY)" ]; then \
		$(DIST)/host-zizpanel sign-manifest --key $(RELEASE_KEY) \
			--in $(RELDIR)/manifest.json --out $(RELDIR)/manifest.json.sig; \
	else \
		echo "==> 跳过签名（没有私钥）。清单可用于手动上传流程。"; \
		rm -f $(RELDIR)/manifest.json.sig; \
	fi
	@# 留一份"GitHub 版清单"：GitHub Release 那边要的是指向 GitHub 的 url，
	@# 而国内镜像要的是指向镜像的 url —— 同一批包、两份清单，都签同一个私钥。
	@cp $(RELDIR)/manifest.json $(RELDIR)/manifest-github.json
	@cp $(RELDIR)/manifest.json.sig $(RELDIR)/manifest-github.json.sig 2>/dev/null || true
	@# 保留 host-zizpanel：`make mirror-manifest` 还要用它给"镜像版清单"签名。
	@# 真正的清理放在 mirror-manifest 末尾（或 make clean）。
	@echo ""
	@echo "发布产物："
	@ls -lh $(RELDIR)
	@echo ""
	@echo "校验和："
	@shasum -a 256 $(RELDIR)/*.tar.gz | sed 's|$(RELDIR)/||'
	@echo ""
	@echo "面板「在线升级」需要把 manifest.json 与 manifest.json.sig 一起放到升级源目录。"

.PHONY: host-zizpanel
host-zizpanel: ## 构建本机版（只用来给清单签名；make release 也会用到它）
	@go build -trimpath -o $(DIST)/host-zizpanel ./cmd/zizpanel

.PHONY: mirror-manifest
mirror-manifest: host-zizpanel ## 重新生成"指向自建镜像"的清单（发布到国内镜像用；签名后一并上传）
	@# 为什么需要单独一步：make release 产出的清单里的 url 指向 GitHub Releases，
	@# 而国内无代理时 GitHub 直连不通 —— 面板能读到清单却下不动包。
	@# 这个目标把 url 换成自建镜像，签名不变（同一把发布私钥）。
	@test -f $(RELDIR)/zizpanel_$(VERSION)_darwin_arm64.tar.gz || (echo "先跑 make release（要发布包）"; exit 1)
	@python3 tools/make-manifest.py --version $(VERSION) --dir $(RELDIR) \
		--base-url "$(MIRROR_BASE_URL)" --notes-file "$(NOTES_FILE)"
	@set -e; \
	if [ -f "$(RELEASE_KEY)" ]; then \
		$(DIST)/host-zizpanel sign-manifest --key $(RELEASE_KEY) \
			--in $(RELDIR)/manifest.json --out $(RELDIR)/manifest.json.sig; \
		echo "==> 已签名（镜像版清单）"; \
		rm -f $(DIST)/host-zizpanel; \
	else \
		echo "!! 没有 $(RELEASE_KEY)，无法签名"; exit 1; \
	fi

.PHONY: publish-mirror
publish-mirror: mirror-manifest ## 生成镜像版清单并打印"上传到国内镜像"的命令（不自动上传）
	@echo ""
	@echo "把下面这些文件放到 $(MIRROR_BASE_URL) ："
	@echo "  manifest.json  manifest.json.sig  install.sh"
	@echo "  zizpanel_$(VERSION)_darwin_arm64.tar.gz  zizpanel_$(VERSION)_darwin_amd64.tar.gz"
	@echo "  zizpanel_latest_darwin_arm64.tar.gz  zizpanel_latest_darwin_amd64.tar.gz"
	@echo ""
	@echo "同时建 download/$(VERSION)/ 与 download/latest/ 放同样的包（install.sh 的固定 URL 用），"
	@echo "并在站点根放指向 latest 的符号链接。例："
	@echo "  scp $(RELDIR)/manifest.json* $(RELDIR)/zizpanel_*_darwin_*.tar.gz install.sh \\"
	@echo "      <user>@<host>:<站点根>/zizpanel/"

.PHONY: seed-nas-brew
seed-nas-brew: ## 把 brew 瓶（含依赖闭包）预置到镜像的按需缓存里（需 MIRROR=<你的镜像基址> ARGS="python@3.11"）
	@# 为什么要这个目标：镜像的 /brew 是**按需**缓存，装机那一刻上游有没有货决定成败。
	@# 脚本会自己校验 sha256 与 `X-Cache: HIT`，任一不过就非 0 退出（不许谎报"已预置"）。
	@bash tools/seed-nas-brew.sh $(ARGS)

.PHONY: mirror-public
mirror-public: host-zizpanel ## 生成"指向公网镜像"的清单（url 用 download/<版本>/ 布局）并签名
	@# 为什么要单独一份：GitHub/线上镜像把包平铺在同一层，而公网镜像按
	@# download/<版本>/ 分目录。签名覆盖 manifest 原始字节，**事后改 url 会让签名失效**，
	@# 所以只能在生成时用另一套 url 模板、另签一次。
	@# 基址取自 internal/upgrade/source.go 的 MirrorSource（不写第二份内网/公网地址）。
	@test -n "$(PUBLIC_MIRROR_URL)" || { echo "!! 读不到 internal/upgrade/source.go 的 MirrorSource"; exit 1; }
	@test -f $(RELDIR)/zizpanel_$(VERSION)_darwin_arm64.tar.gz || (echo "先跑 make release（要发布包）"; exit 1)
	@python3 tools/make-manifest.py --version $(VERSION) --dir $(RELDIR) \
		--arches "$(ARCHS)" --base-url "$(PUBLIC_MIRROR_URL)/download/{version}/{name}" --notes-file "$(NOTES_FILE)"
	@# 发布产物是要发给用户的：清单里出现任何 RFC1918 地址就当场失败（2026-09-20 用户要求）。
	@if grep -En '\b(192\.168\.[0-9]{1,3}\.[0-9]{1,3}|10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3})\b' $(RELDIR)/manifest.json; then \
		echo "!! 清单里出现内网地址（发布给所有人用的产物绝不允许）：$(RELDIR)/manifest.json"; exit 1; \
	else \
		echo "    ✓ 清单里没有内网地址（基址 $(PUBLIC_MIRROR_URL)）"; \
	fi
	@set -e; \
	if [ -f "$(RELEASE_KEY)" ]; then \
		$(DIST)/host-zizpanel sign-manifest --key $(RELEASE_KEY) \
			--in $(RELDIR)/manifest.json --out $(RELDIR)/manifest.json.sig; \
		echo "==> 已签名（公网镜像版清单，版本 $(VERSION)）"; \
	else \
		echo "!! 没有 $(RELEASE_KEY)，无法签名"; exit 1; \
	fi
	@echo ""
	@echo "同步到你自己镜像机上的 $(PUBLIC_MIRROR_URL)/（地址/账号由调用者提供）："
	@echo "  make publish-nas"

.PHONY: mirror-nas
mirror-nas: mirror-public ## 旧名字（兼容旧文档）；新名字是 mirror-public
	@:

.PHONY: publish-nas
publish-nas: ## 把当前版本 + 公网镜像版清单推送到你自己的镜像机（NAS_HOST/NAS_USER/NAS_ROOT + NAS_PASS 或 ssh 密钥）
	@test -n "$(NAS_PASS)" || { echo "需要镜像机口令：make publish-nas NAS_PASS='...'"; exit 1; }
	@test -n "$(NAS_HOST)" || { echo "需要镜像机地址：make publish-nas NAS_HOST=<你自己的镜像机>"; exit 1; }
	@test -n "$(NAS_USER)" || { echo "需要镜像机用户：make publish-nas NAS_USER=<镜像机用户>"; exit 1; }
	@test -n "$(NAS_ROOT)" || { echo "需要镜像目录：make publish-nas NAS_ROOT=<镜像上的 zizpanel 目录>"; exit 1; }
	@command -v sshpass >/dev/null 2>&1 || { echo "需要 sshpass（brew install hudochenkov/sshpass/sshpass）"; exit 1; }
	@echo "==> 上传 install.sh / 清单 / 发布包到 $(NAS_HOST):$(NAS_ROOT)"
	@sshpass -p '$(NAS_PASS)' rsync -az --no-perms --no-owner --no-group \
		-e "ssh -o StrictHostKeyChecking=accept-new" \
		$(RELDIR)/zizpanel_$(VERSION)_darwin_arm64.tar.gz \
		$(RELDIR)/zizpanel_$(VERSION)_darwin_amd64.tar.gz \
		$(RELDIR)/zizpanel_latest_darwin_arm64.tar.gz \
		$(RELDIR)/zizpanel_latest_darwin_amd64.tar.gz \
		$(RELDIR)/manifest.json $(RELDIR)/manifest.json.sig install.sh \
		$(NAS_USER)@$(NAS_HOST):$(NAS_ROOT)/
	@sshpass -p '$(NAS_PASS)' ssh -o StrictHostKeyChecking=accept-new $(NAS_USER)@$(NAS_HOST) \
		"set -e; cd $(NAS_ROOT); \
		 mkdir -p download/$(VERSION) download/latest; \
		 cp -f zizpanel_$(VERSION)_darwin_arm64.tar.gz zizpanel_$(VERSION)_darwin_amd64.tar.gz download/$(VERSION)/; \
		 cp -f zizpanel_latest_darwin_arm64.tar.gz zizpanel_latest_darwin_amd64.tar.gz download/latest/; \
		 ln -sfn download/$(VERSION)/zizpanel_$(VERSION)_darwin_arm64.tar.gz zizpanel_$(VERSION)_darwin_arm64.tar.gz; \
		 ln -sfn download/$(VERSION)/zizpanel_$(VERSION)_darwin_amd64.tar.gz zizpanel_$(VERSION)_darwin_amd64.tar.gz; \
		 ls -l download/$(VERSION) | head -4"
	@echo ""
	@echo "验证（从这台机器）：curl -sI $(PUBLIC_MIRROR_URL)/manifest.json"

.PHONY: deploy
deploy: ## 一条命令发布：release + 推镜像机（单流）+ 升级本机 + 验证（生产机不许碰，见 AGENTS 铁律 5）
	@NAS_HOST='$(NAS_HOST)' NAS_USER='$(NAS_USER)' NAS_ROOT='$(NAS_ROOT)' \
	 MIRROR_URL='$(MIRROR_URL)' \
	 ZP_PASS='$(ZP_PASS)' NAS_PASS='$(NAS_PASS)' \
	 SKIP_CHECK='$(SKIP_CHECK)' SKIP_BUILD='$(SKIP_BUILD)' \
	 bash tools/deploy.sh

.PHONY: sync-apps
sync-apps: ## 把应用安装包同步到你自己的镜像机（apps/<应用>/<版本>/<文件名>；地址由调用者提供）
	@test -n "$(NAS_HOST)" || { echo "需要镜像机地址：make sync-apps NAS_HOST=<你自己的镜像机>"; exit 1; }
	@NAS_HOST="$(NAS_HOST)" NAS_USER="$(NAS_USER)" NAS_ROOT="$(NAS_APPS_ROOT)" \
	 NAS_PASS="$(NAS_PASS)" MIRROR_BASE_URL="$(APPS_MIRROR_URL)" \
	 bash tools/sync-nas-apps.sh $(SYNC_ARGS)

# ---------------------------------------------------------------- 版本号 --
.PHONY: bump
bump: ## 按约定递增版本号（+0.0.1，到 10 后 +0.1）
	@python3 tools/bump-version.py
	@python3 -m py_compile tools/bump-version.py

.PHONY: version
version: ## 显示当前版本号
	@python3 tools/bump-version.py --show

# ------------------------------------------------------------------ 部署 --
.PHONY: install
install: dev ## 本机安装（需要管理员权限）
	sudo bash install.sh

.PHONY: uninstall
uninstall: ## 卸载面板（保留数据）
	sudo bash /opt/zizpanel/uninstall.sh

.PHONY: status
status: ## 查看本机面板状态
	@/opt/zizpanel/bin/zizpanel status 2>/dev/null || echo "面板未安装"

.PHONY: logs
logs: ## 实时查看面板日志
	@tail -f /opt/zizpanel/logs/panel-$$(date +%Y%m%d).log

.PHONY: clean
clean: ## 清理构建产物
	@rm -rf $(DIST) $(LOCAL_ROOT)
	@echo "已清理"

# --------------------------------------------------- 应用市场审计（第 3 层） --
#
# 用户的原话："如果以后每加一个应用都要一点一点慢慢调试，那这个应用市场就
# 没什么实用价值了。" 这两个 target 就是那个"高效工作流"的入口：
#
#   make market-audit            # 一条命令审全部 27 个应用（联网，有缺口非零退出）
#   make market-audit ARGS="--only squoosh"   # 加新应用时只审一个
#   make market-audit ARGS="--json"           # 机器可读
#   make market-audit-offline    # 静态门禁（不联网，秒级；等价于 TestMarket 那组单测）
#
# 静态门禁本身已经在 `make check` 里生效（走 go test 的 TestMarketInvariantsHold /
# TestMarketDeclarationsCoverCatalogExactly），所以这里不把它并进 check ——
# check 是硬门禁，而在线审计会因为真实缺口（比如镜像站上还没同步某个包）红灯，
# 那份红灯是**体检报告**，不是"代码写错了"。
.PHONY: market-audit
market-audit: ## 应用市场审计：一条命令审全部应用（在线，有缺口非零退出）
	@tools/market-audit.sh $(ARGS)

.PHONY: market-audit-offline
market-audit-offline: ## 应用市场静态门禁：只查声明完整性/不变量（不联网）
	@tools/market-audit.sh --offline

.PHONY: market-audit-verify
market-audit-verify: ## 真机安装验收（会真的装软件）：ARGS="<base_url> <user> <pass>"
	@tools/market-audit.sh --install -- $(ARGS)

.PHONY: ui-audit
ui-audit: ## UI 审计（需要先 make run-local；查溢出 / 无名按钮 / 重复设置项）
	@ZP_PASS='$(ZP_PASS)' node tools/ui-audit.mjs http://127.0.0.1:$(LOCAL_PORT)/$(LOCAL_SUFFIX) $(SHOTS)
