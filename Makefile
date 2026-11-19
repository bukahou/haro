# haro 构建入口。bin/ 不进 git。
#   make build        生成 bin/haro-server（服务端，纯 Go；记事本、网络诊断等自研工具都内置在里面，v5）；bin/haro（终端，cgo：
#                     音频库 miniaudio 随源码编译，不用装系统库，v4 D10）
#                     与 bin/haro-asr（cgo，依赖 sherpa-onnx 原生库；单独运行，服务端按 HARO_ASR_URL 连它）
#                     登记声纹用终端的 bin/haro -enroll <名字>（经服务端登记）
#   make run          构建后同时运行 haro-asr、服务端（后台）和终端（前台）；Ctrl+C 一起停
#   make run-server    只运行 haro-asr 和服务端
#   make run-terminal 只运行终端（服务端要已经在跑）
#   make test         全部测试
#   make check-darwin 在 Linux 上检查 Mac 版能否满足平台层契约（GOOS=darwin 类型检查纯 Go 部分；含 cgo 的音频只能在 Mac 上编）
#   make version      打印这次构建的版本号
#
# 发版：合并到 main 后由 GitHub Actions 自动完成（.github/workflows/release.yml）——终端各平台程序进 GitHub Release，
#   服务端镜像推 Docker Hub，再改部署仓库里的镜像标签。本机不推送镜像。
#
# 常驻（systemd 用户服务，跟着桌面会话启停；单元文件在 deploy/systemd/）
#   make install-terminal-service  装上并启用终端（以后登录桌面就自动启动）。服务端跑在集群里时只装这个
#   make install-server-services   装上并启用 haro-asr 和服务端（服务端跑在这台机器上时）
#   make uninstall-services        停掉并卸下装了的全部
#   make deploy                    构建后重启这台机器上装了的服务（换上新版本）
#   开关：systemctl --user start|stop haro.service；日志：journalctl --user -u haro -f
.PHONY: build run run-server run-terminal test check-darwin install-terminal-service install-server-services uninstall-services deploy check-not-running check-no-manual-run image version

TERMINAL_SERVICES := haro.service
SERVER_SERVICES   := haro-asr.service haro-server.service
SERVICES          := $(SERVER_SERVICES) $(TERMINAL_SERVICES)

# 版本号：v + VERSION 文件 + 提交号（和 GitHub Release、镜像的标签同一个字符串）；有没提交的改动时加 -dirty
VERSION  := v$(shell cat VERSION)-$(shell git rev-parse --short=7 HEAD)$(shell git diff --quiet HEAD || echo -dirty)
LDFLAGS  := -X github.com/bukahou/haro/internal/buildinfo.Version=$(VERSION)

# 服务端镜像（deploy/server/Dockerfile）：标签 = 版本号；有没提交的改动时拒绝（镜像要能对上源码）
IMAGE      ?= bukahou/haro-server
IMAGE_TAG  ?= $(VERSION)
MODELS_DIR ?= $(HOME)/.local/share/haro

version:
	@echo $(VERSION)

build:
	go build -ldflags '$(LDFLAGS)' -o bin/haro ./cmd/haro            # cgo：终端音频用 miniaudio
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/haro-server ./cmd/haro-server
	go build -ldflags '$(LDFLAGS)' -o bin/haro-asr ./cmd/haro-asr

# 几个程序在同一个进程组：Ctrl+C 都会收到；前台的先退出时 trap 收掉后台的
# 开发时直接跑：服务在跑就会抢麦克风和端口 ⇒ 先停服务
check-not-running:
	@if systemctl --user is-active --quiet $(SERVICES); then \
		echo "haro 服务正在运行（会抢麦克风和端口）：先 systemctl --user stop $(SERVICES)，跑完再 start"; exit 1; fi

run: check-not-running build
	@./bin/haro-asr & asr=$$!; ./bin/haro-server & server=$$!; \
	trap 'kill $$server $$asr 2>/dev/null; wait $$server $$asr 2>/dev/null' EXIT INT TERM; \
	./bin/haro

run-server: check-not-running build
	@./bin/haro-asr & asr=$$!; \
	trap 'kill $$asr 2>/dev/null; wait $$asr 2>/dev/null' EXIT INT TERM; \
	./bin/haro-server

run-terminal: check-not-running build
	./bin/haro

test:
	go test ./...

# 反过来：装服务 / 换版本前，手动跑的（make run）要先停，否则服务抢不到端口和摄像头
# 平台层：Mac / Windows 版的「各系统不同的部分」和服务层在 Linux 上也能做类型检查
check-darwin:
	GOOS=darwin CGO_ENABLED=0 go vet ./internal/platform/osdiff/ ./internal/platform/camera/ ./internal/terminal/
	GOOS=windows CGO_ENABLED=0 go vet ./internal/platform/osdiff/ ./internal/platform/camera/ ./internal/terminal/

check-no-manual-run:
	@for program in haro-asr haro-server; do \
		if pgrep -u "$$USER" -x $$program >/dev/null && ! systemctl --user is-active --quiet $$program.service; then \
			echo "有手动启动的 $$program 在跑（make run）：先在那个终端按 Ctrl+C 停掉"; exit 1; fi; \
	done

install-terminal-service: check-no-manual-run build
	systemctl --user enable --now $(addprefix $(CURDIR)/deploy/systemd/,$(TERMINAL_SERVICES))

install-server-services: check-no-manual-run build
	systemctl --user enable --now $(addprefix $(CURDIR)/deploy/systemd/,$(SERVER_SERVICES))

uninstall-services:
	-systemctl --user disable --now $(SERVICES)

deploy: check-no-manual-run build
	@for unit in $(SERVICES); do \
		if systemctl --user cat $$unit >/dev/null 2>&1; then systemctl --user restart $$unit && echo "已重启 $$unit"; \
		else echo "跳过 $$unit（这台机器没装；服务端在集群里时正常）"; fi; \
	done

# 构建服务端镜像（只在本机试，不推送：推送只走 CI，v6）。模型不在仓库里：用命名构建上下文 models 传进去，
# 目录结构见 deploy/server/models.lock；没有模型时先 scripts/fetch-models.sh $(MODELS_DIR)。看镜像里有什么：
#   docker run --rm --entrypoint sh $(IMAGE):$(IMAGE_TAG) -c 'ls -R /opt/haro /etc/haro'
image:
	@git diff --quiet HEAD || { echo "有没提交的改动：先提交，镜像标签要能对上源码"; exit 1; }
	docker buildx build --platform linux/amd64 -f deploy/server/Dockerfile --build-arg VERSION=$(IMAGE_TAG) \
		--build-context models=$(MODELS_DIR) -t $(IMAGE):$(IMAGE_TAG) --load .
