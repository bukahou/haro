# haro 架构

haro 是一个家用语音助手：家里每个房间放一台**终端**（麦克风、音箱、可选摄像头），所有终端连到同一个**服务端**。
终端只管收音、放音、选设备；听懂、思考、说什么都在服务端里。

```
 终端 haro（每台机器一个）                          服务端 haro-server（家里一个）
 ┌───────────────────────────┐   WebSocket     ┌──────────────────────────────────────────┐
 │ 麦克风 → 上传音频          │ ─────────────▶ │ 语音识别 haro-asr（单独的进程 / 容器）       │
 │ 音箱  ← 播放回答           │ ◀───────────── │   VAD · 说完判断 · 唤醒词 · 声纹            │
 │ 摄像头 → 画面（可选）       │  Realtime 事件 │ 对话（按人分开）→ 大模型 → 工具              │
 │ 看守：断线重连、设备随时重选 │                │ 语音合成（GPT-SoVITS / Piper）             │
 └───────────────────────────┘                 └──────────────────────────────────────────┘
```

## 程序

| 程序 | 作用 | 编译 |
|---|---|---|
| `cmd/haro` | 终端。`haro -enroll <名字>` 登记声纹（录音经服务端登记） | cgo（音频库 miniaudio 随源码编译，不用装系统库）；摄像头在 macOS 上要 ffmpeg（Linux 不用） |
| `cmd/haro-server` | 服务端 | 纯 Go |
| `cmd/haro-asr` | 本地语音识别服务（流式转写，协议照 OpenAI Realtime 的转写会话）。单独运行：集群里和服务端是同一个 Pod 的两个容器，开发机上各一个 systemd 用户单元；服务端按 `HARO_ASR_URL` 连它 | cgo（sherpa-onnx） |

## 终端和服务端之间的协议

兼容 OpenAI Realtime API 的事件协议（WebSocket），只用其中的一个子集；haro 自己的扩展事件以 `haro.` 开头
（例如终端上报现在用的麦克风和音箱）。服务端和 haro-asr 之间的转写会话用同一份定义。见 `internal/realtimeproto`：

- 线上的 JSON 结构只定义一处（`wire.go`），编解码只有一处（`codec.go`）；
- 由它生成 JSON Schema（`haro.realtime.v2.schema.json`），测试比对两者，结构改了而文件没更新就失败；
- 错误事件带稳定的错误码（`errors.go`），调用方按码判断，不看说明文字。
- 握手时用 WebSocket 子协议协商版本（`haro.realtime.v2`）：报了不认识的 haro 版本、或者旧版终端没报版本，服务端拒绝握手，
  响应里给错误码和它支持的版本；什么都不报的当普通 Realtime 客户端（只用标准事件）。终端被拒后提示一次，隔几分钟才再试；
- 两端各自每 10 秒 ping 一次，5 秒内等不到 pong 就断开（`internal/heartbeat`），终端的看守随即重连。
- 终端的设备能力（现在是「拍一帧」）走 MCP：终端是 MCP 服务端，服务端是 MCP 客户端，消息装在 `haro.mcp.message` 事件里，
  跑在同一条连接上（`internal/mcpbridge`；工具的名字、参数、结果、错误码只在 `internal/terminaltools` 定义一处）。
  模型调用 `look_at_camera` 时，服务端才让终端打开摄像头拍一帧，摄像头平时关着；摄像头插拔时终端增删这个工具，服务端随之给不给模型看画面。
- 一轮对话由服务端开始：语音前端判定说完时，服务端马上开始回答（先发标准的 `response.created`），识别和准备回答同时进行；
  终端不上传画面、也不发 `response.create`，只负责播放（`create_response` 设成 false 的普通 Realtime 客户端照旧自己请求）。

## 服务端（`internal/brain` 与周边）

服务端核心只依赖接口（`internal/brain/ports.go`），具体实现由 `cmd/haro-server` 组装注入：

| 接口 | 实现 |
|---|---|
| `StreamingRecognizer` | `internal/speech`：按 `HARO_ASR_URL` 连 haro-asr（或其他照 OpenAI Realtime 转写会话的服务） |
| `ChatModel` | `internal/llm`：ollama（本地）、Claude API 或 OpenAI 兼容接口（云端）；主备切换（`FallbackChatModel`） |
| `SpeechSynthesizer` | `internal/speech`：GPT-SoVITS（边合成边播放）、Piper |
| `Toolbox` | `internal/tools`：内置工具 + 第三方 MCP 服务 |
| `ConversationStore` | `internal/conversationstore`：每人一个文件 |
| `Briefer` | 每轮补充给模型的即时情况：现在几号几点、主人家大致在哪（城市级）、额度提醒、后台调查的任务板 |

- **坏了要出声**：服务端和 haro-asr 之间的识别流断了会自己重开（带退避），重开后把「醒着 / 正在播放」重新告诉它；
  断开超过 10 秒才提示终端一次（`haro.notice` 的 `transcription_lost`，终端播「断开」那一声），重开后再提示一次（`transcription_restored`）。
  存活检查 `/livez` 只看进程；就绪检查 `/readyz` 看识别、合成、模型凭证、账本，结果由后台定期检查（`internal/readiness`）。
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
| `get_weather` | 天气（Open-Meteo，不用密钥）；家的位置可以配坐标，或按公网 IP 推定。Open-Meteo 查不到时改用日本气象厅（见下） |
| `look_at_camera` | 看终端摄像头的画面：调用时才经 MCP 让终端拍一帧（终端的工具清单里有「拍一帧」时才提供） |
| `switch_persona` | 换性格（有两个以上性格时才提供） |
| `web_search` | 联网搜一次（Tavily 远程 MCP）；设了密钥才提供 |
| `start_research` / `research_status` / `cancel_research` | 后台调查（见下）；设了搜索密钥才提供 |

### 天气的备用来源（`internal/weather`）

Open-Meteo 连不上或报错时，日本境内的地点改查日本气象厅网站的数据（`HARO_WEATHER_BACKUP=jma`，默认开；`none` 关掉）：

- 内容：短期预报（三天，天气是气象厅的日语原文）+ 周间预报（到第 7 天）+ 附近观测站的实况（最近的站缺测时用下一个）。
  没有体感温度与降水量预报；交给模型的结果里注明来源。
- 地点 → 预报地区按坐标自动找：国土地理院的地址反查（坐标 → 市町村）+ 气象厅的地区表（政令市的区归到所属的市；
  一个市分成几块时取代表气温点最近的那块）。⇒ 要查的坐标也会发给国土地理院。家所在的地区在启动时找好并写进日志。
- 气象厅的这些数据文件是网站自己用的，不是正式公开的接口，格式可能变 ⇒ 只做备用；地区表下载一次后在进程里缓存。
- 连接（TCP、TLS 握手）各最多等 1.5 秒，建立不起来不重试，尽快交给备用；连上后卡住才重试一次。
- 数据出处：天气 [Open-Meteo](https://open-meteo.com/)；备用 [気象庁](https://www.jma.go.jp/)（気象庁ホームページ）；
  地址反查 [国土地理院](https://www.gsi.go.jp/)。冒烟测试：`go test -tags integration -run Live ./internal/weather/`

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
- **平台层** `internal/platform`：门面 `OpenLocalDevices`；`audio`（miniaudio）、`camera`（要看时才打开摄像头拍一帧：Linux 用 pion/mediadevices，预热到曝光稳定再取；ffmpeg 是备用，macOS 现在用它）、
  `osdiff`（各操作系统的差异：设备的接口类型怎么认、摄像头怎么找、向摄像头要什么格式和分辨率；按编译标签自动选，`make check-darwin` 在 Linux 上检查 macOS 版能否编译）。
- 摄像头画面和具体摄像头无关：`HARO_CAMERA_SIZE` 是想要的分辨率，Linux 上先列出摄像头支持的格式（同一摄像头各格式能出的分辨率不同，
  驱动遇到要不到的会悄悄换），能出就明确要它，出不了就要宽度够的里面最小的；`HARO_FRAME_WIDTH`（默认 1280）是送给模型的**最大**宽度，
  画面更窄就原样送、不放大。启动时日志记下向摄像头要了什么（「📷 向摄像头要画面」）和实际送给模型多大（「📷 送给模型的画面」）。
- 提示音（`assets/tones`：内置的不含人声；可以用 `HARO_TONES_DIR` 换成自己录的）：服务端断开 / 恢复、
  对话窗口结束去休眠（窗口默认 40 秒）、服务端的提示（例如后台调查查完了）。

## 配置

全部来自环境变量（`internal/config/config.go` 里每一项都有说明）。通常写在一个 env 文件里，
systemd 用户服务从 `~/.config/haro/haro.env` 读取（`deploy/systemd/`）。凭证（云端模型、搜索服务的密钥）只放在环境变量里，没有默认值。

云端模型（`HARO_LLM_PROVIDER=cloud`）的接口格式由 `HARO_CLOUD_LLM_API` 选：

- `anthropic`（默认）：Claude API（官方 Go SDK `anthropic-sdk-go`）。`HARO_CLOUD_LLM_MODEL` 例如 `claude-haiku-5-5`；
  思考关掉求快，回答力度用 `HARO_CLOUD_LLM_EFFORT`（聊天、摘要）/ `HARO_RESEARCH_EFFORT`（后台调查）；
  开着提示词缓存，缓存读写的单价用 `HARO_CLOUD_LLM_PRICE_CACHE_READ` / `_WRITE` 记账（不设就按普通输入价，记多不记少）。
  凭证：`HARO_ANTHROPIC_API_KEY`；不填时交给 SDK 自己找——在 Kubernetes 里可以用 Workload Identity Federation
  （服务账号令牌换短期令牌，不放长期密钥），配法见 Anthropic 文档「Use WIF with Kubernetes」。
  不要用 `ANTHROPIC_API_KEY` 放 haro 的 key：开发机上 Claude Code 也读这个变量。
  服务端全进程只建一个 Claude 客户端（`llm.ClaudeAPI`），聊天、摘要、调查共用，HTTP 客户端也只用一个：
  SDK 按「客户端 × HTTP 客户端」各存一份令牌，而集群的服务账号令牌只能换一次，多一份就会被拒（401）。
- `openai`：任意 OpenAI 兼容的 `/chat/completions`（`HARO_CLOUD_LLM_URL`、`HARO_CLOUD_LLM_API_KEY`）。

**按规则选模型**（可选，`HARO_STRONG_LLM_MODEL`，例如 `claude-sonnet-5-5`；和云端模型同一个接口与凭证）：
换哪个模型写在代码里（`llm.RouterChatModel`）：看画面按规则、重答由前台模型认出主人在纠正后标记，不让模型凭自己有没有把握决定换不换。`HARO_STRONG_LLM_USE` 选用在哪些地方：

- `vision`：看摄像头画面。`HARO_STRONG_LLM_VISION_MODE` 选怎么用：
  - `expert`（默认）：**前台始终是云端模型**，它当幕后专家（`internal/vision`）。模型调用 `look_at_camera` 时，
    画面和主人这一轮的原话交给专家（不带性格和对话记录，输入约 1.6k token）；主人问到画面上看不全的信息时专家自己联网查（最多 2 次）——
    只有它看得到画面，才能按「画面比搜到的资料可靠」取舍。前台拿到专家的判断（文字）后用自己的口吻转述，判断留在历史里；
    专家出错或超时时退回把画面直接交给前台。按 `vision` 用途单独记账。
  - `takeover`：带画面的那次请求直接换它回答（画面只在本轮输入里，下一轮自然回到云端模型）
- `correction`：主人纠正（说它答错了、让它重新想）或明说要想清楚时，前台模型调用内置工具 `ask_stronger_model`（`brain/escalation.go`），
  这一轮剩下的请求交给它重答。「是不是在纠正」由模型认（任何语言、任何说法），不靠写死的词表；工具说明要求一认出就交、不争辩
  （实测：只写「纠正时调用」时，Haiku 觉得自己没错就会争辩、不交）
- `research`：后台调查由云端模型搜索、读网页，搜完把资料交给它写结论（不给工具）

直接换它回答时，它出错或等太久（`HARO_STRONG_LLM_TIMEOUT`）由云端模型接手。单价 `HARO_STRONG_LLM_PRICE_*` 必填（月度上限要靠它折算）。
Claude 各模型「关掉思考」的写法不同：`HARO_CLOUD_LLM_THINKING` / `HARO_STRONG_LLM_THINKING`，Haiku 5.5 用 `disabled`，
Sonnet 5.5 只认 `between_tools`（两次工具调用之间会返回几句进度说明，haro 不回传；实测后续请求照样被接受）。

真实接口的冒烟测试（花钱，默认不跑）：`HARO_ANTHROPIC_API_KEY=… go test -tags integration -run AnthropicLive -v ./internal/llm/`；
看画面的专家：`go test -tags integration -run LiveExpert -v ./internal/vision/`（用法见该测试开头）；
认不认得出纠正：`go test -tags integration -run LiveAskStronger -v ./internal/brain/`。
看图小评测（花钱，默认不跑；样本放在仓库外）：`internal/llm/vision_eval_test.go`（`-tags eval`，先用 `HARO_EVAL_DRY_RUN=1` 看要花多少）。

模型文件（识别、VAD、说完判断、唤醒词、声纹）不在仓库里，路径都由配置指定；服务端镜像里已经带着
（清单、来源与许可证：`deploy/server/models.lock`、`deploy/server/MODELS.md`；本机下载：`scripts/fetch-models.sh ~/.local/share/haro`）。

## 构建与运行

```
make build            # bin/haro、bin/haro-server、bin/haro-asr
make test
make run              # 前台同时跑 haro-asr、服务端和终端
make install-terminal-service  # 把终端装成 systemd 用户服务（跟着桌面会话启停：麦克风权限只给登录中的桌面会话）
make install-server-services   # 服务端跑在这台机器上时：haro-asr 和服务端各一个 systemd 用户服务
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
  1. 测试 + 终端各平台（列表只在 `ci.yml` 的 `platforms` 一处；现在是 Linux x86_64 / arm64，Mac 版暂停编译）的程序，各在原生机器上编译，用安装脚本装、卸一遍；
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
