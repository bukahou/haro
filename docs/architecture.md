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
- 由它生成 JSON Schema（`haro.realtime.v3.schema.json`），测试比对两者，结构改了而文件没更新就失败；
- 错误事件带稳定的错误码（`errors.go`），调用方按码判断，不看说明文字。
- 握手时用 WebSocket 子协议协商版本（`haro.realtime.v3`）：报了不认识的 haro 版本、或者旧版终端没报版本，服务端拒绝握手，
  响应里给错误码和它支持的版本；什么都不报的当普通 Realtime 客户端（只用标准事件）。终端被拒后提示一次，隔几分钟才再试；
- 两端各自每 10 秒 ping 一次，5 秒内等不到 pong 就断开（`internal/heartbeat`），终端的看守随即重连。
- 终端的设备能力（现在是「拍一帧」）走 MCP：终端是 MCP 服务端，服务端是 MCP 客户端，消息装在 `haro.mcp.message` 事件里，
  跑在同一条连接上（`internal/mcpbridge`；工具的名字、参数、结果、错误码只在 `internal/terminaltools` 定义一处）。
  模型调用 `look_at_camera` 时，服务端才让终端打开摄像头拍一帧，摄像头平时关着；摄像头插拔时终端增删这个工具，服务端随之给不给模型看画面。
- 一轮对话由服务端开始：语音前端判定说完时，服务端马上开始回答（先发标准的 `response.created`），识别和准备回答同时进行；
  终端不上传画面、也不发 `response.create`，只负责播放（`create_response` 设成 false 的普通 Realtime 客户端照旧自己请求）。
  说完方式和积极度的取值只在 `realtimeproto` 定义一处，终端启动时和 haro-asr 收到会话设置时都按它查，写错就明确拒绝；
- 醒 / 睡只由 haro-asr 的唤醒门决定：终端要休眠（对话窗口结束、按回车）或手动唤醒时只发请求（`haro.wake_word.sleep` / `wake`），
  服务端转给 haro-asr，它处理完回 `haro.wake_word.state`，服务端原样转给终端；听到唤醒词时 haro-asr 自己醒、报 `haro.wake_word.detected`。
  服务端和终端记的醒 / 睡只照这两个事件改，发请求时不改；终端等不到回复（`assistant.Timing.WakeConfirm`）就当没切成，醒着的话过一个对话窗口再试。
  识别流断开时服务端记下终端要的状态，重开后告诉新的流，由它回复确认。
  semantic_vad 判定没说完时会把下一段并进同一句，最长并到 30 秒（`asrserver.Timing.MergeLimit`），再长就按说完交出、后面的另成一句。

## 服务端（`internal/brain` 与周边）

服务端核心只依赖接口（`internal/brain/ports.go`），具体实现由 `cmd/haro-server` 组装注入：

| 接口 | 实现 |
|---|---|
| `StreamingRecognizer` | `internal/speech`：按 `HARO_ASR_URL` 连 haro-asr（或其他照 OpenAI Realtime 转写会话的服务） |
| `ChatModel` | `internal/llm`：Claude API（官方 SDK）、OpenAI 兼容接口（官方 openai-go；本地 Ollama 走它的兼容端点），本地和云端按配置二选一、不自动互相顶替。请求可以「工具照给、这轮不许调用」（提示词缓存不失效）；输出到上限（或上下文满了）时明确报「被截断」；流要么交代为什么停，要么报错：连接正常关掉却没收到结束标志按连不上算，停下的原因认不出或没交代的不当成说完；需要预热的模型另实现 `ModelWarmer`。三个适配跑同一组契约测试（`internal/llm/contract_test.go`） |
| `SpeechSynthesizer` | `internal/speech`：GPT-SoVITS（边合成边播放）、Piper |
| `Toolbox` | `internal/tools`：内置工具 + 第三方 MCP 服务 |
| `ConversationStore` | `internal/conversationstore`：SQLite（goose 迁移）；每一轮带说话的时间，发给模型时每轮前标日期和时刻 |
| `Briefer` | 每轮补充给模型的即时情况：现在几号几点、主人家大致在哪（城市级）、额度提醒、后台调查的任务板 |

- **观测**：OpenTelemetry（`HARO_OTLP_ENDPOINT`，OTLP/HTTP）。大模型相关的按语义约定 `gen_ai.*` 记：每次请求一个 span
  「chat <模型>」（提供方、模型、用量含缓存读写、为什么停；不知道的不写）和标准指标 `gen_ai.client.operation.duration`、`gen_ai.client.token.usage`；
  一轮回答记成 `invoke_agent`（代理名，交接记成事件），工具调用记成 `execute_tool`。haro 自己的（说完到第一个字、合成、打断、唤醒）用 `haro.` 前缀。
- **坏了要出声**：服务端和 haro-asr 之间的识别流断了会自己重开（带退避），重开后把「醒着还是睡着 / 正在播放」重新告诉它；
  说到一半断开时，服务端替丢掉的那句补一个「说完」，这一轮按识别失败结束，终端照常收尾；
  断开超过 10 秒才提示终端一次（`haro.notice` 的 `transcription_lost`，终端播「断开」那一声），重开后再提示一次（`transcription_restored`）。
  haro-asr 接受识别流的设置后回 `session.updated`，服务端等到它才算打开；不接受的（请求类的错误码，例如要 semantic_vad 而它没配
  说完判断模型）不再重开，把同一个错误码告诉终端一次、提示 `transcription_lost`。haro-asr 同时进行的转写会话有上限
  （`asrserver.DefaultLimits`），满了回 `too_many_sessions`，服务端照断开重开那样退避再试。
  存活检查 `/livez` 只看进程；就绪检查 `/readyz` 看识别、合成、模型凭证、账本，结果由后台定期检查（`internal/readiness`）。
  其中识别那一项探的是 haro-asr 自己的 `/readyz`：它在 1 秒内真的识别完一小段静音才算就绪（识别器卡住时跟着失败）。
- **对话跟着人走**：开了声纹时，服务端认出是谁在说话，用这个人自己的对话历史；换终端、断线重连、服务端重启都接得上。
  发给模型的只有最近几轮加一段摘要：更早的对话在后台交给代理「摘要」压成一段，放进系统提示词（`internal/brain/summary.go`）；
  一轮里请求模型带的上下文（模型报的输入 token）到了 `HARO_CONTEXT_COMPACT_TOKENS`，较早的几轮提前压缩（模型服务不报用量时做不到，只按轮数截断，用量日志里写明）。
  完整的对话记录另存一份（对话库的 `transcript` 表，每人每轮一行、按轮次编号，只增不删），留作以后的长期记忆。
- **代理**（`internal/agents`）：交给模型做的事按「代理」定义——名字、模型档（`base` = 云端模型 / 本地模型，`strong` = 更强的模型；
  具体的模型 ID 和思考写法只在配置里）、系统提示词、能用的工具（允许名单）、步数上限与死循环判定、记账用途。
  每个代理一个 markdown 文件（开头 YAML 是配置，正文是系统提示词；正文用 `{{.名字}}` 引用代码里定义的值，例如情绪标记的列表）。
  程序里带一套（`internal/agents/builtin/`）：前台（主对话）、强模型（接手）、看画面（子任务）、摘要（子任务）、额度判断（子任务：月度上限到了以后判断要不要临时提高）；
  `HARO_AGENTS_DIR` 里同名的文件替换程序里带的那份（启动日志写明用了哪份；字段写错、文件名不认识、工具名不认识都起不来）。
  只有部分代理认的配置项写在不认的代理上也起不来：`timeout`（调用一次最多等多久：看画面、摘要）、`max_searches`（看画面最多联网查几次）。
  交出去只有一个入口（`brain.EnterAgent`）：**接手**带着完整的对话历史回答这一轮剩下的部分（主人纠正时交给强模型），
  **子任务**不带历史、交回一份结果（看画面、摘要）；代理链最多 3 层。模型只执行这一步给了它的工具（确认、取消除外：它们自己核对并说明为什么不执行）。
- **提示词的结构**（从前到后）：
  1. 前台的基础规则（身份、能力、规矩；`internal/agents/builtin/front.md`），`HARO_SYSTEM_PROMPT` 接在后面；
     接着是按这一轮实际提供的工具生成的工具介绍（工具怎么用只写在工具自己的说明里）；
  2. 主人资料（`HARO_PROFILE_DIR`，外部注入：全家一份 `household.md` + 每人一份，按声纹认出的名字选；模板见 `assets/profile.example`）；
  3. 当前性格；
  4. 对话摘要。

  这几段很少变；每轮会变的内容（时间、位置、谁在说话、任务板）附在这一轮主人的话前面——
  支持提示词缓存的模型按开头复用，本地模型也不用每轮从头处理；历史里只存主人的原话。
  主人资料每轮都发给模型：只写事实与偏好，不写住址、健康、证件这类敏感信息。
- **多台终端**：几台终端同时听到唤醒词时，服务端短暂等一下，选一台回答（离得近、正在播放的优先；`wake_arbiter.go`）。
- **性格**：`assets/personas/*.json`，可以用语音切换；回答里的情绪标记决定语音合成用哪段参考录音。

### 工具

自研工具注册在服务端进程内的 MCP 服务上（参数、结果由结构体定义，调用前校验；每个工具标明只读 / 会改数据 / 会破坏）；第三方服务只接远程 Streamable HTTP。
会破坏的工具（和提高月度上限）执行时要带着主人的确认（确认闸放进去的 `brain.Confirmation`），工具箱和额度那一层自己核对，见不到就不执行。

| 工具 | 说明 |
|---|---|
| `save_note` / `search_notes` / `delete_note` / `list_deleted_notes` / `restore_note` | 记事本（SQLite，时间存 UTC 的 Unix 秒）；按编号删，删掉的可以恢复 |
| `confirm_action` / `cancel_action` | 会破坏的操作（删记事、提高月度上限）先存成待确认的操作（有时效），主人算不算同意由模型判断后调用；有会破坏的工具时一直提供（工具定义不变，提示词缓存不失效），系统提示词的工具介绍里不列 |
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
- 调查只用云端模型（本地小模型调工具不可靠）。

### 费用（`internal/spending`）

大模型和搜索按量计费时：每次调用都记账（token、搜索积分，按单价与汇率折成本地货币），按用途分开记
（每个代理定义里写自己的用途：前台 `chat`、强模型 `strong`、看画面 `vision`、摘要 `summary`；另有快查 `search`、后台调查 `research`），
按天、按月封顶。账本写不进时（例如数据卷满了）那笔先留在内存里，之后每次放行前先补写，补写不成就和读不出账本一样不再付费调用
（不让上限因为没记上而失效），写进去了自动恢复。超过月度上限后不再回答，念一句固定的话（到上限了、下个月一号恢复）；主人可以口头临时提高这个月的上限
（`HARO_MONTHLY_RAISE_YEN`，每次加多少），要过确认闸（登记过的声纹 + 有时效的确认）。听懂「要提高」靠一次很小的模型调用
（不带历史，只给提高上限和确认的工具，模型说的话不念），它不受额度管，按天限次数（`HARO_OVER_BUDGET_DAILY_CALLS`）。

### 连不上云端时（最低能力）

适配层把连不上（网络错误、等不到开始回答、超时、认证失败、限流、服务端出错；流开始之后才发来的错误事件按错误码同样归类）标成带类型的错误。这时不调用任何模型，
无论主人说什么，都念一句固定的话：连不上 + 现在的时间（中日英各一份）。连不上一次就熔断：之后马上告诉主人，
不用每句都等超时；30 秒后放一个请求去试，通了就恢复，下一轮由模型顺带交代刚才断过。

云端模型的单价来自公共的模型价格目录（`internal/pricecatalog`，默认 [models.dev](https://models.dev)，`HARO_MODEL_PRICES_*`）：
服务端启动时拉取（含缓存读写的单价、按上下文大小分的档），拉到的那一家存一份到 `HARO_MODEL_PRICES_CACHE`，
下次拉不到时用它；都没有、或目录里没有用到的模型时服务端起不来（费用上限要靠单价折算，不按 0 记）。
汇率（`HARO_USD_JPY`）和搜索积分的价格目录里没有，照旧在配置里写。

## 终端（`internal/terminal` 与 `internal/platform`）

分层，层与层之间只通过接口：

- **服务层** `internal/terminal`、`internal/assistant`：看守（连服务端 → 等麦克风 → 运行，出错就退回去重来）、
  选设备的规则（只看设备的接口类型：有别的麦克风时不用蓝牙麦克风，因为蓝牙耳机当麦克风会切到通话模式、音质下降）。
- **平台层** `internal/platform`：门面 `OpenLocalDevices`；`audio`（miniaudio）、`camera`（要看时才打开摄像头拍一帧：Linux 用 pion/mediadevices，预热到曝光稳定再取；ffmpeg 是备用，macOS 现在用它）、
  `osdiff`（各操作系统的差异：设备的接口类型怎么认、摄像头怎么找、向摄像头要什么格式和分辨率；按编译标签自动选，`make check-darwin`、`make check-windows` 在 Linux 上检查 macOS、Windows 版能否编译）。
  Linux 上都看系统报的结构化信息、不按名字猜：音频设备的接口类型和回录源取 PipeWire / PulseAudio 的设备属性（`pactl -f json`，
  所以终端要装 pulseaudio-utils；没有时照系统默认选麦克风、认不出蓝牙）；摄像头是按 V4L2 查询能列出视频采集格式的节点（元数据节点不算）。
- 摄像头画面和具体摄像头无关：`HARO_CAMERA_SIZE` 是想要的分辨率，Linux 上先列出摄像头支持的格式（同一摄像头各格式能出的分辨率不同，
  驱动遇到要不到的会悄悄换），能出就明确要它，出不了就要宽度够的里面最小的，同一个分辨率帧率高的优先；`HARO_FRAME_WIDTH`（默认 1280）是送给模型的**最大**宽度，
  画面更窄就原样送、不放大。启动时日志记下向摄像头要了什么（「📷 向摄像头要画面」）和实际送给模型多大（「📷 送给模型的画面」）。
- 提示音（`assets/tones`：内置的不含人声；可以用 `HARO_TONES_DIR` 换成自己录的）：服务端断开 / 恢复、
  对话窗口结束去休眠（窗口默认 40 秒）、服务端的提示（例如后台调查查完了）。

## 配置

全部来自环境变量（`internal/config/config.go` 里每一项都有说明）。通常写在一个 env 文件里，
systemd 用户服务从 `~/.config/haro/haro.env` 读取（`deploy/systemd/`）。凭证（云端模型、搜索服务的密钥）只放在环境变量里，没有默认值。
样例（只有占位值）：服务端 `deploy/server/server.env.example`，终端 `deploy/systemd/terminal.env.example`；
终端和服务端各读各的，写在同一个 env 文件里也互不影响（已删除的配置项也只查自己那边的）。

云端模型（`HARO_LLM_PROVIDER=cloud`）的接口格式由 `HARO_CLOUD_LLM_API` 选：

- `anthropic`（默认）：Claude API（官方 Go SDK `anthropic-sdk-go`）。`HARO_CLOUD_LLM_MODEL` 例如 `claude-haiku-5-5`；
  思考关掉求快，回答力度用 `HARO_CLOUD_LLM_EFFORT`（聊天、摘要）/ `HARO_RESEARCH_EFFORT`（后台调查）；
  开着提示词缓存，缓存读写按模型价格目录里的单价分开记账。
  凭证：`HARO_ANTHROPIC_API_KEY`；不填时只认配齐的 Workload Identity Federation（`ANTHROPIC_FEDERATION_RULE_ID`、
  `ANTHROPIC_ORGANIZATION_ID`、`ANTHROPIC_IDENTITY_TOKEN_FILE`：在 Kubernetes 里用服务账号令牌换短期令牌，不放长期密钥，
  配法见 Anthropic 文档「Use WIF with Kubernetes」），没配齐就起不来。不走 SDK 的默认凭证链：
  环境里的 `ANTHROPIC_API_KEY`（开发机上是 Claude Code 的）不会被拿来用。
  服务端全进程只建一个 Claude 客户端（`llm.ClaudeAPI`），聊天、摘要、调查共用，HTTP 客户端也只用一个：
  SDK 按「客户端 × HTTP 客户端」各存一份令牌，而集群的服务账号令牌只能换一次，多一份就会被拒（401）。
- `openai`：任意 OpenAI 兼容的 `/chat/completions`（`HARO_CLOUD_LLM_URL`、`HARO_CLOUD_LLM_API_KEY`）。

**更强的模型**（可选，`HARO_STRONG_LLM_MODEL`，例如 `claude-sonnet-5-5`；和云端模型同一个接口与凭证）：
用 `strong` 档的代理（强模型、看画面）用它。什么时候交给它由前台的工具和规则定（看画面按配置、重答由前台模型认出主人在纠正后交出去），
不让模型凭自己有没有把握决定换不换。`HARO_STRONG_LLM_USE` 选用在哪些地方：

- `vision`：看摄像头画面。`HARO_STRONG_LLM_VISION_MODE` 选怎么用：
  - `expert`（默认）：**前台始终是云端模型**，它当幕后专家（`internal/vision`）。模型调用 `look_at_camera` 时，
    画面和主人这一轮的原话交给专家（不带性格和对话记录，输入约 1.6k token）；主人问到画面上看不全的信息时专家自己联网查（最多 2 次；没配搜索时提示词里不提、也不给搜索工具）——
    只有它看得到画面，才能按「画面比搜到的资料可靠」取舍。前台拿到专家的判断（文字）后用自己的口吻转述，判断留在历史里；
    专家出错或超时时退回把画面直接交给前台。按 `vision` 用途单独记账。
  - `takeover`：画面要交给前台看时，这一轮剩下的部分交给强模型接手（画面只在本轮输入里，下一轮自然回到前台）
- `correction`：主人纠正（说它答错了、让它重新想）或明说要想清楚时，前台模型调用内置工具 `ask_stronger_model`（`brain/escalation.go`），
  这一轮剩下的部分交给强模型接手重答（按 `strong` 用途单独记账）。「是不是在纠正」由模型认（任何语言、任何说法），不靠写死的词表；工具说明要求一认出就交、不争辩
  （实测：只写「纠正时调用」时，Haiku 觉得自己没错就会争辩、不交）
- `research`：后台调查由云端模型搜索、读网页，搜完把资料交给它写结论（不给工具）

接手时它出错或等太久（`HARO_STRONG_LLM_TIMEOUT`）由前台的模型接着回答。单价同样来自模型价格目录（目录里没有它就起不来）。
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
