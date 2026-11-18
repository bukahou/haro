# haro 架构

haro 是一个家用语音助手：家里每个房间放一台**终端**（麦克风、音箱、可选摄像头），所有终端连到同一个**服务端**。
终端只管收音、放音、选设备；听懂、思考、说什么都在服务端里。

```
 终端 haro（每台机器一个）                          服务端 haro-server（家里一个）
 ┌───────────────────────────┐   WebSocket     ┌──────────────────────────────────────────┐
 │ 麦克风 → 上传音频          │ ─────────────▶ │ 语音识别 haro-asr（子进程）                 │
 │ 音箱  ← 播放回答           │ ◀───────────── │   VAD · 说完判断 · 唤醒词 · 声纹            │
 │ 摄像头 → 画面（可选）       │  Realtime 事件 │ 对话（按人分开）→ 大模型 → 工具              │
 │ 看守：断线重连、设备随时重选 │                │ 语音合成（GPT-SoVITS / Piper）             │
 └───────────────────────────┘                 └──────────────────────────────────────────┘
```

## 程序

| 程序 | 作用 | 编译 |
|---|---|---|
| `cmd/haro` | 终端。`haro -enroll <名字>` 登记声纹（录音经服务端登记） | cgo（音频库 miniaudio 随源码编译，不用装系统库）；摄像头需要 ffmpeg |
| `cmd/haro-server` | 服务端 | 纯 Go |
| `cmd/haro-asr` | 本地语音识别服务，由服务端作为子进程启动；也可以单独运行，提供 OpenAI 兼容的转写接口 | cgo（sherpa-onnx） |

## 终端和服务端之间的协议

兼容 OpenAI Realtime API 的事件协议（WebSocket），只用其中的一个子集；haro 自己的扩展事件以 `haro.` 开头
（例如终端上报现在用的麦克风、音箱、摄像头）。见 `internal/realtimeproto`。

## 服务端（`internal/brain` 与周边）

服务端核心只依赖接口（`internal/brain/ports.go`），具体实现由 `cmd/haro-server` 组装注入：

| 接口 | 实现 |
|---|---|
| `SpeechRecognizer` / `StreamingRecognizer` | `internal/speech`：本地 haro-asr、OpenAI 兼容的转写服务 |
| `ChatModel` | `internal/llm`：ollama（本地）、OpenAI 兼容接口（云端）；主备切换（`FallbackChatModel`） |
| `SpeechSynthesizer` | `internal/speech`：GPT-SoVITS（边合成边播放）、Piper |
| `Toolbox` | `internal/tools`：内置工具 + 第三方 MCP 服务 |
| `ConversationStore` | `internal/conversationstore`：每人一个文件 |
| `Briefer` | 每轮补充给模型的即时情况：现在几号几点、主人家大致在哪（城市级）、额度提醒、后台调查的任务板 |

- **对话跟着人走**：开了声纹时，服务端认出是谁在说话，用这个人自己的对话历史；换终端、断线重连、服务端重启都接得上。
  只保留最近几轮；更早的对话在后台压成一段摘要，放进系统提示词（`internal/brain/summary.go`）。
- **提示词的结构**（从前到后）：
  1. 内置的基础提示词（身份、能力、规矩；`brain.DefaultInstructions`）——永远生效；`HARO_SYSTEM_PROMPT` 只能在后面追加；
  2. 主人资料（`HARO_PROFILE_DIR`，外部注入：全家一份 `household.md` + 每人一份，按声纹认出的名字选；模板见 `assets/profile.example`）；
  3. 当前性格；
  4. 对话摘要。

  这几段很少变；每轮会变的内容（时间、位置、谁在说话、任务板）附在这一轮主人的话前面——
  支持提示词缓存的模型按开头复用，本地模型也不用每轮从头处理；历史里只存主人的原话。
  主人资料每轮都发给模型：只写事实与偏好，不写住址、健康、证件这类敏感信息。
- **多台终端**：几台终端同时听到唤醒词时，服务端短暂等一下，选一台回答（离得近、正在播放的优先；`wake_arbiter.go`）。
- **性格**：`assets/personas/*.json`，可以用语音切换；回答里的情绪标记决定语音合成用哪段参考录音。

### 工具

自研工具内置在服务端里；**MCP 只用来接第三方服务**（本地子进程或远程 Streamable HTTP）。

| 工具 | 说明 |
|---|---|
| `get_current_time` | 现在的时间 |
| `save_note` / `search_notes` / `list_recent_notes` / `delete_note` | 记事本（SQLite）；删除前要口头确认 |
| `check_network` / `network_interfaces` | 网络诊断（查的是服务端所在机器的网络） |
| `get_weather` | 天气（Open-Meteo，不用密钥）；家的位置可以配坐标，或按公网 IP 推定 |
| `look_at_camera` | 看终端摄像头的画面（终端有摄像头时才提供） |
| `switch_persona` | 换性格（有两个以上性格时才提供） |
| `web_search` | 联网搜一次（Tavily 远程 MCP）；设了密钥才提供 |
| `start_research` / `research_status` / `cancel_research` | 后台调查（见下）；设了搜索密钥才提供 |

### 后台调查（`internal/research`）

需要多搜几次、读几篇资料的问题，服务端派一个后台任务，立刻回话，主人可以接着聊别的：

- 任务由程序强制上限：时间、轮数、搜索次数、token，先到先停；到上限后不再给工具，用已有资料写结论（标明没查完）。
- 任务和结论存在 SQLite 里；每轮对话的系统提示词里带着「任务板」，下次说话时先告诉主人结论。
- 调查必须先搜：模型想不搜就下结论时会被退回去。
- 查完时，服务端给主人最近说话的那台终端发一个提示（`haro.notice`），终端响一声「叮咚」（睡着也响）；
  主人叫醒后，服务端先说结论。服务端还不能主动开口说话。
- 调查用的模型可以单独配置（例如聊天用本地模型、调查用云端模型）。

### 费用（`internal/spending`）

大模型和搜索按量计费时：每次调用都记账（token、搜索积分，按配置的单价与汇率折成本地货币），
按天、按月封顶。超过月度上限后改用本地模型；没有本地模型时直接告诉主人额度用完了。

## 终端（`internal/terminal` 与 `internal/platform`）

分层，层与层之间只通过接口：

- **服务层** `internal/terminal`、`internal/assistant`：看守（连服务端 → 等麦克风 → 运行，出错就退回去重来）、
  选设备的规则（只看设备的接口类型：有别的麦克风时不用蓝牙麦克风，因为蓝牙耳机当麦克风会切到通话模式、音质下降）。
- **平台层** `internal/platform`：门面 `OpenLocalDevices`；`audio`（miniaudio）、`camera`（ffmpeg 取帧）、
  `osdiff`（各操作系统的差异：设备的接口类型怎么认、摄像头怎么找；按编译标签自动选，`make check-darwin` 在 Linux 上检查 macOS 版能否编译）。
- 提示音（`assets/tones`：内置的不含人声；可以用 `HARO_TONES_DIR` 换成自己录的）：服务端断开 / 恢复、
  对话窗口结束去休眠（窗口默认 40 秒）、服务端的提示（例如后台调查查完了）。

## 配置

全部来自环境变量（`internal/config/config.go` 里每一项都有说明）。通常写在一个 env 文件里，
systemd 用户服务从 `~/.config/haro/haro.env` 读取（`deploy/systemd/`）。凭证（云端模型、搜索服务的密钥）只放在环境变量里，没有默认值。

模型文件（识别、VAD、说完判断、唤醒词、声纹）不在仓库里，路径都由配置指定；服务端镜像里已经带着
（清单、来源与许可证：`deploy/server/models.lock`、`deploy/server/MODELS.md`；本机下载：`scripts/fetch-models.sh ~/.local/share/haro`）。

## 构建与运行

```
make build            # bin/haro、bin/haro-server、bin/haro-asr
make test
make run              # 前台同时跑服务端和终端
make install-services # 装成 systemd 用户服务（跟着桌面会话启停：麦克风权限只给登录中的桌面会话）
make image            # 在本机试建服务端镜像（deploy/server/Dockerfile；模型用构建上下文 models 传进去；不推送）
make version          # 这次构建的版本号
```

## 版本与发版（v6）

- **版本号** = `v` + 仓库根目录的 `VERSION` + 提交号前 7 位，例如 `v0.5.0-1a2b3c4`。GitHub Release 的标签、
  服务端镜像的标签、程序里的版本（`haro -version`、启动日志）是同一个字符串。`VERSION` 只在阶段性的大改时调。
- **终端和服务端握手时互报版本**（HTTP 头 `X-Haro-Version`）：版本不同时两边的日志都会提醒升级终端。
- **合并前的检查**（`.github/workflows/ci.yml`；日常开发在 `dev`，推 `dev` 和开 PR 时跑）：测试、终端三个平台的编译和安装脚本，
  再试建一次服务端镜像（不推送；和发版用同一份构建步骤 `.github/actions/server-image`）—— Dockerfile、基础镜像的问题合并前就能发现。
- **合并到 `main` 就发版**（`.github/workflows/release.yml`）：
  1. 测试 + 终端三个平台（Linux x86_64 / arm64、macOS Apple 芯片）的程序，各在原生机器上编译，用安装脚本装、卸一遍；
  2. 程序发到 GitHub Release（附 `SHA256SUMS`、`install.sh`、来源证明），设为 latest；`v*` 的 Release 只留最近 20 个；
  3. 服务端镜像：模型从本仓库的 Release 下载并按 `models.lock` 校验 → 构建 → Trivy 扫描（有可修的 CRITICAL
     或疑似凭证就不推）→ 推 Docker Hub `bukahou/haro-server`（版本号 + latest）；
  4. 改部署仓库里的镜像标签（生产环境要人工 SYNC 才会换版本）。
- **模型的 Release**：原发布方随时可能删文件 ⇒ 模型第一次从原发布方下载、校验后，放进本仓库的 Release
  （`models-v1`，由 `.github/workflows/models.yml` 建：推 `models-v*` 标签，或在 Actions 页面手动运行）；之后构建只从这里下载。
- **依赖更新**：Dependabot 每周检查 Go 依赖、基础镜像（Dockerfile 里钉了 digest）、GitHub Actions，有新版就开 PR 到 `dev`（`.github/dependabot.yml`）；
  镜像构建时还会打上 Debian 的安全补丁。
- **安装终端**：`curl -fsSL https://github.com/bukahou/haro/releases/latest/download/install.sh | sh -s -- --server-url http://服务端地址:59130`
  （装到 `~/.local/bin/haro`，配置在 `~/.config/haro/haro.env`，Linux 装成 systemd 用户服务、macOS 装成 LaunchAgent；
  再跑一次就是升级；`--uninstall` 卸载）。冒烟测试：`scripts/test-install.sh`。

## 安全说明

- **服务端和语音识别服务的接口没有鉴权**，只应该跑在你信任的网络里（例如家里的局域网，不要暴露到公网）。
- 声纹是生物特征：只存在服务端本机的数据目录里。
- 声音档案（`assets/voices/example.json` 是格式示例）请只用你有权使用的声音。
