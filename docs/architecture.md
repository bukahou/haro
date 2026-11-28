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
| `cmd/haro` | 终端。`haro -enroll <名字>` 登记声纹（要读的句子和录音都经服务端交给 haro-asr）；`haro -pair` 配对这台终端（存下设备凭证） | cgo（音频库 miniaudio 随源码编译，不用装系统库）；摄像头在 macOS 上要 ffmpeg（Linux 不用） |
| `cmd/haro-server` | 服务端 | 纯 Go |
| `cmd/haro-asr` | 本地语音识别服务（流式转写，协议照 OpenAI Realtime 的转写会话）。单独运行：集群里和服务端是同一个 Pod 的两个容器，开发机上各一个 systemd 用户单元；服务端按 `HARO_ASR_URL` 连它 | cgo（sherpa-onnx） |

## 终端和服务端之间的协议

兼容 OpenAI Realtime API 的事件协议（WebSocket），只用其中的一个子集；haro 自己的扩展事件以 `haro.` 开头
（例如终端上报现在用的麦克风和音箱）。服务端和 haro-asr 之间的转写会话用同一份定义。见 `internal/realtimeproto`：

- 线上的 JSON 结构只定义一处（`wire.go`），编解码只有一处（`codec.go`）；
- 由它生成 JSON Schema（`haro.realtime.v4.schema.json`），测试比对两者，结构改了而文件没更新就失败；
- 错误事件带稳定的错误码（`errors.go`），调用方按码判断，不看说明文字。
- 握手时用 WebSocket 子协议协商版本（`haro.realtime.v4`）：报了不认识的 haro 版本、或者旧版终端没报版本，服务端拒绝握手，
  响应里给错误码和它支持的版本；什么都不报的当普通 Realtime 客户端（只用标准事件）。终端被拒后提示一次，隔几分钟才再试；
- **终端的身份**：配对过的终端握手时带设备凭证（`Authorization: Bearer`，haro 发的随机串、库里只存 SHA-256），服务端认出是哪台、属于哪个账号；
  凭证无效（在 App 里移除了）回 401（错误码 `invalid_credential`），⛔ 不退回成「没带」，终端提示重新配对、隔几分钟才再试。
  没带凭证的终端照旧能连、归管理员账号（过渡期，只在局域网；第 21 段把语音连接开到公网时改成必须带）。普通 Realtime 客户端的 `Authorization` 是它自己的 API key，不看。
  配对照 RFC 8628：`haro -pair` 调 `POST /v1/terminal-registration` 拿配对码（8 位、10 分钟、用一次）和轮询凭据、显示配对码，
  定时 `POST /v1/terminal-registration/poll` 问结果；人在 App 里输入配对码 = 绑到自己的账号，管理员批准 = 归一个新的匿名账号。
  批准后终端第一次来问时才生成设备凭证、只交出一次（库里不存明文），存进 `~/.config/haro/device-credential`（600）。
  别的账号的终端只认本人的声音：别的声音是这个账号的访客，不算声纹确认，别人的声纹名也不发给这台终端（`brain.Residents`）。
  App 里移除 = 凭证作废 + 断开它正连着的会话。
- 两端各自每 10 秒 ping 一次，5 秒内等不到 pong 就断开（`internal/heartbeat`），终端的看守随即重连。
- 服务端的对话会话、haro-asr 的转写会话同时最多 `realtimeproto.MaxSessions` 条（两边同一个数：每条对话会话在 haro-asr 占一个转写会话），满了新连接在握手前回 503（错误码 `too_many_sessions`）。
- 照办不了的请求整条拒绝、不悄悄忽略：`session.update` 声明的输入格式不是 16 kHz 单声道，或识别流打开以后再改说完判断、唤醒门；
  `conversation.item.truncate` 缺 `audio_end_ms`（或为负）、带了 `item_id`（haro 不发回答的 item_id，只截最近一次回答）。
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
- 唤醒词只写在 haro-asr 的关键词文件一处：haro-asr 在转写会话的 `session.updated` 里报出（`haro.wake_word.phrases`），
  服务端用对话会话的 `session.updated` 转给终端（变了才再发），终端日志里的唤醒提示照它写；客户端的 `session.update` 带唤醒词就拒绝。
  声纹登记要读的句子也由 haro-asr 按它生成（`GET /v1/voiceprint-enrollment/sentences`，服务端转发）。
- 服务端会自己开口（v4）：到点的提醒不等主人说话，服务端发带 `haro.unprompted`（缘由，例如 `reminder`）的 `response.created`，
  接着照普通回答发文字、声音和 `response.done`。只在这台终端没在播、服务端没在答、主人没在说的时候开口（回答事件里没有回答编号，
  声音不能混在一起），所以终端不管能不能打断都报「开始播放 / 播完了」（`haro.playback.started` / `stopped`）。
  终端在主循环里照播回答的方式播（可以喊唤醒词打断），播完醒着等一个对话窗口，主人可以不叫唤醒词直接接话。
- 模型判断主人要结束这次对话时（`end_conversation`），`response.done` 带 `haro.end_conversation`：终端播完就请语音前端休眠，被打断就不睡。
  semantic_vad 判定没说完时会把下一段并进同一句，最长并到 30 秒（`asrserver.Timing.MergeLimit`），再长就按说完交出、后面的另成一句。

## 服务端（`internal/brain` 与周边）

服务端核心只依赖接口（`internal/brain/ports.go`），具体实现由 `cmd/haro-server` 组装注入：

| 接口 | 实现 |
|---|---|
| `StreamingRecognizer` | `internal/speech`：按 `HARO_ASR_URL` 连 haro-asr（或其他照 OpenAI Realtime 转写会话的服务） |
| `ChatModel` | `internal/llm`：Claude API（官方 SDK）、OpenAI 兼容接口（官方 openai-go；本地 Ollama 走它的兼容端点），本地和云端按配置二选一、不自动互相顶替。请求可以「工具照给、这轮不许调用」（提示词缓存不失效）；输出到上限（或上下文满了）时明确报「被截断」；流要么交代为什么停，要么报错：连接正常关掉却没收到结束标志按连不上算，停下的原因认不出或没交代的不当成说完；需要预热的模型另实现 `ModelWarmer`。三个适配跑同一组契约测试（`internal/llm/contract_test.go`） |
| `SpeechSynthesizer` | `internal/speech`：GPT-SoVITS（边合成边播放）、Piper。两种实现跑同一组契约测试（`internal/speech/synthesizer_contract_test.go`） |
| `Toolbox` | `internal/tools`：内置工具 + 外部 MCP 服务（配置文件里列出） |
| `ConversationStore` | `internal/conversationstore`：SQLite（goose 迁移）；每一轮带说话的时间，发给模型时每轮前标日期和时刻 |
| `Briefer` | 每轮补充给模型的即时情况：现在几号几点、主人家大致在哪（城市级）、额度提醒、后台调查的任务板、错过的提醒 |

- **观测**：OpenTelemetry（OTLP/HTTP，`internal/telemetry`）。往哪发：标准的 `OTEL_EXPORTER_OTLP_ENDPOINT`（或分信号的）设了时以它为准，
  否则用 `HARO_OTLP_ENDPOINT`，都没有就只打日志；终端、服务端各报自己的 service.name（`haro-terminal` / `haro-server`），
  `OTEL_SERVICE_NAME`、`OTEL_RESOURCE_ATTRIBUTES` 可以覆盖；退出时最多等 1 秒把没发出的发出去。大模型相关的按语义约定 `gen_ai.*` 记：每次请求一个 span
  「chat <模型>」（提供方、模型、用量含缓存读写、为什么停；不知道的不写）和标准指标 `gen_ai.client.operation.duration`、`gen_ai.client.token.usage`；
  一轮回答记成 `invoke_agent`（代理名，交接记成事件），工具调用记成 `execute_tool`。haro 自己的（说完到第一个字、合成、打断、唤醒）用 `haro.` 前缀。
- **坏了要出声**：服务端和 haro-asr 之间的识别流断了会自己重开（带退避），重开后把「醒着还是睡着 / 正在播放」重新告诉它；
  说到一半断开时，服务端替丢掉的那句补一个「说完」，这一轮按识别失败结束，终端照常收尾；
  断开超过 10 秒才提示终端一次（`haro.notice` 的 `transcription_lost`，终端播「断开」那一声），重开后再提示一次（`transcription_restored`）。
  haro-asr 接受识别流的设置后回 `session.updated`，服务端等到它才算打开；不接受的（请求类的错误码，例如要 semantic_vad 而它没配
  说完判断模型）不再重开，把同一个错误码告诉终端一次、提示 `transcription_lost`。haro-asr 同时进行的转写会话有上限
  （`asrserver.DefaultLimits`），满了回 `too_many_sessions`，服务端照断开重开那样退避再试。
  存活检查 `/livez` 只看进程；就绪检查 `/readyz` 看识别、合成、模型凭证、账本，结果由后台定期检查（`internal/readiness`）；
  启动后第一轮出错（一起启动的 haro-asr 常晚一两秒才监听）只记 INFO，第二轮还出错才记 WARN（`/readyz` 照实返回）。
  其中识别那一项探的是 haro-asr 自己的 `/readyz`：它在 1 秒内真的识别完一小段静音才算就绪（识别器卡住时跟着失败）。
- **对话跟着人走**：开了声纹时，服务端认出是谁在说话，用这个人自己的对话历史；换终端、断线重连、服务端重启都接得上。
  很短的话常算不出声纹：这次醒着期间（这台终端）认出过谁就沿用他的对话，但不算声纹确认（确认不了危险操作）；还没认出过谁就进全家共用的对话。
  发给模型的是历史加一段摘要：默认不按轮数截（`HARO_MAX_HISTORY_TURN` 默认 0），每一轮前面带着那一轮的日期、星期和时间
  （和每轮的「现在是……」同一种写法，隔天的话分得清）；更早的对话在后台交给代理「摘要」压成一段，放进系统提示词（`internal/brain/summary.go`）；
  一轮里请求模型带的上下文（模型报的输入 token）到了 `HARO_CONTEXT_COMPACT_TOKENS`（默认 8 万：守每次请求的输入低于 10 万，超过后单价更高），
  较早的几轮提前压缩（模型服务不报用量的那一轮改按 6 轮截）；对话闲了 `HARO_CONTEXT_IDLE_COMPACT`（默认 1 小时，和对话的提示词缓存一样长）
  也在后台同样提前压缩：缓存过期后再开口，第一次请求不必带着整段旧对话按全价重新处理。
  完整的对话记录另存一份（对话库的 `transcript` 表，每人每轮一行、按轮次编号，只增不删），留作以后的长期记忆。
- **代理**（`internal/agents`）：交给模型做的事按「代理」定义——名字、模型档（`base` = 云端模型 / 本地模型，`strong` = 更强的模型；
  具体的模型 ID 和思考写法只在配置里）、系统提示词、能用的工具（允许名单）、步数上限与死循环判定、记账用途。
  每个代理一个 markdown 文件（开头 YAML 是配置，正文是系统提示词；正文用 `{{.名字}}` 引用代码里定义的值，例如情绪标记的列表）。
  程序里带一套（`internal/agents/builtin/`）：前台（主对话）、强模型（接手）、看画面（子任务）、摘要（子任务）、额度判断（子任务：月度上限到了以后判断要不要临时提高）、
  调查与调查审查（后台调查查资料、核对结论，见下）；
  `HARO_AGENTS_DIR` 里同名的文件替换程序里带的那份（启动日志写明用了哪份；字段写错、文件名不认识、工具名不认识都起不来）。
  只有部分代理认的配置项写在不认的代理上也起不来：`timeout`（调用一次最多等多久：看画面、摘要、调查审查）、`max_searches`（看画面最多联网查几次）。
  交出去只有一个入口（`brain.EnterAgent`）：**接手**带着完整的对话历史回答这一轮剩下的部分（主人纠正时交给强模型），
  **子任务**不带历史、交回一份结果（看画面、摘要）；代理链最多 3 层。模型只执行这一步给了它的工具（确认、取消除外：它们自己核对并说明为什么不执行）。
- **提示词的结构**（从前到后）：
  1. 前台的基础规则（身份、能力、规矩；`internal/agents/builtin/front.md`），`HARO_SYSTEM_PROMPT` 接在后面；
     接着是按这一轮实际提供的工具生成的工具介绍（工具怎么用只写在工具自己的说明里）；
  2. 主人资料（`HARO_PROFILE_DIR`，外部注入：全家一份 `household.md` + 每人一份，按声纹认出的名字选；模板见 `assets/profile.example`）；
  3. 当前性格；
  4. 对话摘要。

  附上的情况（任务板、额度提醒、错过的提醒）主人听完那一轮才算告诉了；那一轮被打断就下一轮再附上，
  但最多再附上一次（`brain.BriefingCuts`，再附上时说明上次被打断了），又被打断就当告诉过了。
  这几段很少变；每轮会变的内容（时间、离上一次说话过了多久、今天和明天是不是节假日（`HARO_HOLIDAYS` 设了国家时，`internal/holidays`：
  规则用 rickar/cal，库里有全国节假日的 47 国都登记了）、
  位置、谁在说话、任务板）附在这一轮主人的话前面——
  支持提示词缓存的模型按开头复用，本地模型也不用每轮从头处理；历史里只存主人的原话。
  主人资料每轮都发给模型：写事实与偏好（住址、通勤、家里的机器、常用的名字都可以），不写密码、银行卡和账户号、证件号码、
  验证码和密钥；每份最多发一万字（只防误贴大文件），没填的项、没内容的小标题不发。搜索和派调查的工具说明要求不把门牌号、房间号写进搜索词。
- **多台终端**：几台终端同时听到唤醒词时，服务端短暂等一下，选一台回答（离得近、正在播放的优先；`wake_arbiter.go`）。
- **性格**：`assets/personas/*.json`，可以用语音切换；回答里的情绪标记决定语音合成用哪段参考录音。

### 工具

自研工具注册在服务端进程内的 MCP 服务上（参数、结果由结构体定义，调用前校验；每个工具标明只读 / 会改数据 / 会破坏），按组开关
（`HARO_TOOLS`：`memory` 记事与长期记忆、`reminders`、`weather`、`search` 前台的搜索和读网页、`research` 后台调查；不设 = 全开，`none` = 都不要；
组名只在 `internal/config` 定义一处）。看画面的专家查资料用同一个搜索服务（设了密钥就有），不算前台的工具组。

外部的 MCP 服务不改代码就能接上、拿掉：列在 `HARO_MCP_SERVERS_FILE` 里（`internal/tools/remote_servers.go`），格式照 Claude Code 的 `.mcp.json`
（`mcpServers` → 名字 → `type` / `url` / `headers`），另加两项：`tools` 是允许名单（必填，服务那边别的工具不交给模型），
`confirm` 是另外要主人确认的工具。`url`、`headers` 里的 `${名字}` 启动时换成环境变量的值（凭证不写进文件，没设就起不来）。
只支持 Streamable HTTP（服务都单独运行，服务端不在本机拉起程序）。启动时各连一次：连不上不影响启动，后台按退避重连，
连上后它的工具才出现在清单里；用着断了，当次重连再试一次（和联网搜索共用一份连接代码）。
要不要主人确认 = 服务标了会破坏（按 MCP 规范，没标注的、非只读的都算）**或** `confirm` 里列了；配置只能加、不能免。
和内置工具重名就起不来。每次调用写一行日志（服务、工具、用时、成没成功）；外部工具不花钱的不记账。
会破坏的工具（和提高月度上限）执行时要带着主人的确认（确认闸放进去的 `brain.Confirmation`），工具箱和额度那一层自己核对，见不到就不执行。
工具箱执行前还按工具的参数定义校验参数。参数不对的（工具箱拦下的，或工具自己报的 `brain.ErrInvalidArguments`：格式不对、取值之间对不上、
编号指向的东西没有）交给模型时附注叫它改正参数、这一轮就重调；其他错误（连不上、超时、上游一时出错）附注说多半是暂时的。
按错误的类型分，不看报错的文字：进程内的工具经 MCP 交回时，结果的元数据里带着标记，工具箱还原成同样的错误。
模型能触发的动作就是下表这些（会话自己提供的工具和工具箱里的）；「过确认闸」= 执行前要登记过的声音 + 有时效的确认。
这张表由测试和代码核对（`internal/docstest`：工具箱和会话工具的名字、要不要过确认闸），加工具、改标注时跟着改。

| 工具 | 过确认闸 | 说明 |
|---|---|---|
| `remember` / `recall` / `list_memories` / `restore_memory` | | 长期记忆（见下；记事已并进来）：记住、按意思找（记住的事 + 以前的对话）、列出、恢复；记住和恢复只认声纹认出的这句 |
| `forget` | ✓ | 按编号忘掉（软删除，可以恢复） |
| `raise_monthly_cap` | ✓ | 主人口头临时提高这个月的费用上限（每次加 `HARO_MONTHLY_RAISE`）；设了月度上限才提供 |
| `confirm_action` / `cancel_action` | | 会破坏的操作（忘掉记忆、取消提醒、提高月度上限）先存成待确认的操作（有时效），主人算不算同意由模型判断后调用这两个（它们自己核对声纹、时效、参数，必须是下一轮）；有会破坏的工具时一直提供（工具定义不变，提示词缓存不失效），系统提示词的工具介绍里不列 |
| `get_weather` | | 天气（Open-Meteo，不用密钥）；家的位置可以配坐标，或按公网 IP 推定。地名可以带国家代码（`country_code`），只在那个国家里找；同名的其他地方连坐标一起交给模型。Open-Meteo 查不到时改用日本气象厅（见下） |
| `look_at_camera` | | 看终端摄像头的画面：调用时才经 MCP 让终端拍一帧（终端的工具清单里有「拍一帧」时才提供） |
| `switch_persona` | | 换性格（有两个以上性格时才提供） |
| `ask_stronger_model` | | 主人纠正、或明说要想清楚时，前台模型交给更强的模型重答（`HARO_STRONG_LLM_USE` 有 correction 时才提供） |
| `end_conversation` | | 模型判断主人要结束这次对话时调用：这一轮说完，终端播完就休眠（用唤醒门的终端才提供） |
| `set_reminder` / `list_reminders` / `restore_reminder` | | 提醒（见下）：定、查、恢复；定和恢复只认声纹认出的这句 |
| `list_calendar` | | 查日程（日历，见下）：一段时间里的每一次，haro 自己的和系统日历同步进来的 |
| `cancel_reminder` | ✓ | 按编号取消提醒（可以恢复） |
| `web_search` | | 联网搜一次（Tavily 远程 MCP）；设了密钥才提供 |
| `read_web_pages` | | 读搜索结果里的网页正文（一次最多 3 页、每页约 2000 字，按 focus 给相关段落）；和搜索共用每天的积分，设了密钥才提供 |
| `start_research` / `research_status` / `cancel_research` | | 后台调查（见下）；设了搜索密钥才提供 |

### 天气的备用来源（`internal/weather`）

Open-Meteo 连不上或报错时，日本境内的地点改查日本气象厅网站的数据（`HARO_WEATHER_BACKUP=jma`；跟部署的地方有关，没有默认值，不设 = 不用备用）：

- 内容：短期预报（三天，天气是气象厅的日语原文）+ 周间预报（到第 7 天）+ 附近观测站的实况（最近的站缺测时用下一个）。
  没有体感温度与降水量预报；交给模型的结果里注明来源。
- 地点 → 预报地区按坐标自动找：国土地理院的地址反查（坐标 → 市町村）+ 气象厅的地区表（政令市的区归到所属的市；
  一个市分成几块时取代表气温点最近的那块）。⇒ 要查的坐标也会发给国土地理院。家所在的地区在启动时找好并写进日志。
- 气象厅的这些数据文件是网站自己用的，不是正式公开的接口，格式可能变 ⇒ 只做备用；地区表下载一次后在进程里缓存。
- 连接（TCP、TLS 握手）各最多等 1.5 秒，建立不起来不重试，尽快交给备用；连上后卡住才重试一次。
- 数据出处：天气 [Open-Meteo](https://open-meteo.com/)；备用 [気象庁](https://www.jma.go.jp/)（気象庁ホームページ）；
  地址反查 [国土地理院](https://www.gsi.go.jp/)。冒烟测试：`go test -tags integration -run Live ./internal/weather/`

### 长期记忆（`internal/memory`）

v10（2026-11-22 裁定）：记住的事和以前的对话都能按意思找回（「我车停哪了」「上次我们聊过的那个大学」）。

- **记事并进记忆**：记事库（`internal/notes`，`HARO_NOTES_DB`）的每一条标明记给谁（声纹认出的名字；空 = 全家共用，之前记的都算）、
  来源（`owner` 主人让记的 / `model` 模型觉得该记的，由模型判断）、要不要常驻。一个人只看得到、只改得了自己的和全家共用的。
- **由模型调工具写**（`remember` / `forget`）；只有声纹认出的这句能记、恢复，忘掉过确认闸。不记住址、健康、证件、密码一类（写在工具说明里）。
- **常驻记忆**（称呼、忌口这类）每轮附在情况说明里（不进系统提示词，提示词缓存不受影响）：新的在前，最多 15 条、一共 400 字。
- **按意思找**（`recall`）：嵌入模型把文字变成向量（`internal/embedding`：OpenAI 兼容的嵌入接口，`HARO_EMBEDDING_*`；集群里是
  Text Embeddings Inference 跑 `intfloat/multilingual-e5-small`；模型名和 e5 要的「query: / passage: 」前缀都是配置项，程序里没有默认值）。
  向量存在 SQLite（记事库的 `note_vectors`、对话库的 `transcript_vectors`），检索在服务端内存里逐条算余弦相似度（`memory.Index`）：
  家里的规模一次几毫秒，不加依赖、不改构建。⚠️ 对话超过约 10 万轮、或一次检索超过 50 ms 时换 PostgreSQL + pgvector（只换 `Index` 后面的实现）。
  一次给模型最像的记住的事 5 条、以前的对话 3 轮，相关不相关由模型判断。
- **后台补向量**：启动时载入库里的向量；之后每隔 20 秒（记住新的一条时马上）把没算过的记事和对话轮次算上，
  这一轮的文字后来变了（被打断、改成听到的部分）的重算；已有的对话记录也补上。嵌入服务连不上时照样记，向量之后补；就绪检查里有 `embedding`。
- 没设 `HARO_EMBEDDING_URL` 时照样能记、列、忘、恢复，常驻也照常；「查」只给最近记的，以前的对话查不了（启动日志写明）。
- 指标：`haro.memory.embed.duration`、`haro.memory.recall.duration`、`haro.memory.index.entries`。

### 提醒（`internal/reminders`）

「十分钟后叫我」「明早七点提醒我吃药」「每周一提醒倒垃圾」：

- 时间由模型算好交来：`after_seconds`（多少秒以后）或 `at`（RFC 3339，带时区偏移）二选一，重复的加 `rrule`（RFC 5545，
  例如 `FREQ=DAILY`；解析和推算用 rrule-go）。代码只校验（在将来、最远一年、重复的两次之间至少 5 分钟、每人在用的最多 50 条）和推算，
  不理解「明早」「每周一」这样的话。时间存 UTC，按服务端的时区（`TZ`）推算钟点、给模型看。
- 存在记事库的同一个文件里（`reminders` 表；表结构的版本记在自己的 `goose_db_version_reminders` 表，和记事的迁移互不影响）；
  提醒按人记，只能查、取消、恢复自己的。
- 到点时调度（`reminders.Scheduler`，服务端里一个循环：睡到最近一次到点，有变化时被叫醒）把定提醒时存下的那句话交给
  这个人最近说话的那台终端（`brain.NoticeHub.Announce`；他在哪台都没说过话时交给最近连上的那一台）：不经模型直接合成，
  云端模型连不上照样说；合成用不了时终端播提醒的提示音（`reminder.wav`）。说出的话记进他的对话历史（标明是到点提醒），
  主人接着说「再过十分钟提醒我」时模型知道刚才提醒了什么。
- 到点时没有在线的终端（服务端重启、终端断线）：到点后 30 分钟以内终端连上就补说（`reminders.CatchUpWindow`），
  更晚的记成错过，主人下次开口时附在情况说明里告诉他（听完那一轮才算告诉了）。
- 指标：`haro.reminders.next_due`（最近一次到点的时间）、`haro.reminders.spoken`（说出来的，标明是不是改播了提示音、是不是晚了补说的）、
  `haro.reminders.missed`（错过的）。

### 日历（`internal/calendar`）

每个人日程的正本（语音和 App 共用）：标题、开始 / 结束或全天、地点、备注、重复规则（RFC 5545 RRULE）、几个提醒（提前多久）、在终端 / 手机响、
来源（haro 自己的 / 系统日历同步进来的，后者只读）。表和提醒、记事同一个库文件（`HARO_NOTES_DB`）。
- 重复规则的展开、「只改这一次」（排除那一次、另建单次的）、「以后都改」（原来的截到那一次之前、另起一条）只在这里算。
- 终端那边的提醒仍由提醒的调度说：挂在日程上的提醒存成「下一次到点」的单次提醒（`event_id`、`lead_seconds`），响完 / 错过之后问日历要再下一次；
  日程建、改、删时日历重新挂（`ArmEvent` / `DisarmEvent`）。到点说的话：提前 0 是标题，提前 N 分钟用 `phrases.json` 的固定句式。
- 原来的提醒启动时收编成日程；语音定提醒同时建日程（App 里看得到），取消 / 恢复语音定的提醒时日程跟着删 / 恢复；`list_calendar` 给模型查日程。
- 系统日历单向同步：App 交来一个日历在同步范围（client-config）内的全部日程（重复的展开好），按系统的编号增、改，这次没交来的删掉。
- 手机的提醒：`/calendar/alerts?until=` 给出接下来要在手机上响的清单，App 排本地通知。

### 记账（`internal/expenses`）

主人自己的账目（店名、时间、明细行、小计、税、合计、币种、付款方式、分类、说明、原图），存在自己的库（`HARO_EXPENSES_DB`），
和 haro 自己的花费账本（模型、搜索的费用）分开。金额是 micros + ISO 4217（`internal/money`，十进制写法用 `money.ParseAmount` 精确解析）。

- 交小票：请求体就是图片，存成 JPEG（和聊天的图片同一个目录、同一个存法）→ 后台解析（所有人一起同时最多 2 张，一个人同时最多 10 张在解析；
  服务端重启后接着解析）→ 草稿 → 主人确认（可以改任何一项）才入账，原图跟着这笔账走，删账目时一起删。
- 解析交给代理「记账」（`internal/agents/builtin/receipt.md`，记在「看画面」的用途里）：看图后用 `save_receipt` 交回草稿，
  看不清、拿不准的字段名单独交；不是小票时 `readable` 为 false。代码只校验格式（金额、币种、分类键在清单里），不从模型写的文字里找。
- 入账在一个事务里把小票从「解析好了」改成「已入账」：同时确认两次只入账一笔。交上来 30 天还没确认的小票连原图一起删。
- 分类：固定的 12 类（键在代码里一处，名字在 App 的文字里按语言）+ 主人加的；汇总按月（服务端的时区）、按分类、按店，
  合计换成默认货币（没有汇率的币种不算进合计、单独注明），附上个月的合计。

### 后台调查（`internal/research`）

需要多搜几次、读几篇资料的问题，服务端派一个后台任务，立刻回话，主人可以接着聊别的：

- 查资料的是代理「调查」（`internal/agents/builtin/research.md`，云端模型）：自己决定搜什么、读哪几页，查够了用 `submit_conclusion`
  交草稿（结论和来源网址分开交）；调查时按网址记下拿到的资料（读到的正文优先，没读正文的用搜索结果的标题和摘要）。
  系统提示词逐字不变，现在的日期放在任务的消息里。
- 任务由程序强制上限：时间、轮数、搜索次数、累计 token、一次请求的上下文，先到先停；到上限后不再给搜索工具，用已有资料写结论（标明没查完）。
  一次请求的上下文到了 8 万（`HARO_RESEARCH_MAX_CALL_TOKENS`）就收工：累计上限管不住单次，守每次请求的输入低于 10 万（复查也照这个停）。
  时限、搜索次数、token、写结论和复查的时限在 `HARO_RESEARCH_*`（不设时用 `research.DefaultLimits`），轮数默认是代理定义的 `max_steps`
  （设了 `HARO_RESEARCH_MAX_ROUNDS` 时用它）。
- 两个调查代理的模型参数和对话分开设：思考写法 `HARO_RESEARCH_THINKING`（默认 `adaptive`，开着思考）、力度 `HARO_RESEARCH_EFFORT`（默认 `xhigh`）、
  每次输出上限 `HARO_RESEARCH_MAX_OUTPUT_TOKENS`（不设时用代理定义的 `max_output_tokens`，思考也算在里面）；审查用哪个模型 `HARO_RESEARCH_REVIEWER`
  （默认 `base` 云端模型，`strong` 更强的模型）、审一次多久 `HARO_RESEARCH_REVIEW_TIMEOUT`（不设时用定义的 `timeout`）。
  在后台、主人不用等，所以开着思考：查得更细（读的网页更多、会交代来源之间的出入），对应的累计 token、写结论和复查的时限默认值也按开着思考定。
  开着思考时，每一步回答里的思考块跟着回答带到下一步（`brain.ProviderReplay`，内容只有 Claude 的适配层看得懂：
  `llm/anthropic_replay.go`）；思考块只在它之前的内容一字不差时才收，所以只在调查任务之内带，到上限写结论那一次换了工具清单，不带。
- 审查（`HARO_STRONG_LLM_USE` 有 `research` 时）：草稿交给代理「调查审查」（`research_review.md`，模型档见上一条）核对，只给它问题、草稿、
  草稿列出的来源的摘录和现在的日期，不给调查过程（每个来源的摘录和调查时每页看到的一样长，总长有上限；列了却没拿到内容的写明）。
  它用 `submit_review` 交回：通过，或逐条疑点（哪句话、没依据 / 和资料矛盾 / 不准确、依据说的是什么）；只审一次，不改结论。
  有疑点时疑点作为一条消息交回原来的调查对话复查一次（额外的搜索次数、轮数、时间都有上限）：查到依据的保留并补上来源，
  查不到的删掉或说清楚不确定，再交一次，交出了就用新结论。审查出错、超时、没用工具交回时用草稿（原因里写明没审查成）；
  复查没交出新结论时用草稿，任务记成「可能不全」。代码不改结论的文字，也不看疑点说了什么。到上限写出的草稿同样走审查。
  调查和审查都按 `research` 用途记账（受每天的调查额度管）。派出时告诉主人最长要等多久（有审查时加上审查和复查的时间）。
- 任务和结论存在 SQLite 里；每轮附上的情况说明里带着「任务板」：查完了还没告诉的，模型先回答主人这次说的话，
  最后用一句话告诉他查完了、问要不要听结论，他要听再取结论讲（这一句听完就算告诉了，结论随时能问）。
- 调查必须先搜：模型想不搜就下结论时会被退回去。
- 查完时，服务端给主人最近说话的那台终端发一个提示（`haro.notice`），终端响一声「叮咚」（睡着也响）；
  主人叫醒后，haro 先回答他这句，再告诉他查完了、问要不要听。
- 调查只用云端模型（本地小模型调工具不可靠）。

### 费用（`internal/spending`）

大模型和搜索按量计费时：每次调用都记账（token、搜索积分，按单价算出花了多少，**按原始币种记**：服务商按美元收费就记美元），按用途分开记
（每个代理定义里写自己的用途：前台 `chat`、强模型 `strong`、看画面 `vision`、摘要 `summary`、调查与调查审查 `research`；另有快查 `search`，后台调查的搜索也记 `research`），
按天、按月封顶。上限和念给主人的金额用默认货币（`HARO_DEFAULT_CURRENCY`，ISO 4217；跟用的人有关，没有默认值），判断时把账本按币种分开的合计
按参考汇率换成默认货币再比（`internal/money`）。用到云端模型时每月总上限（`HARO_MONTHLY_CAP`）必填、要大于 0（没有默认的「不限」，漏配时起不来），
设了月度上限就必须设账本（`HARO_USAGE_DB`：上限按账本合计，只记在内存里时一重启就清零）。
账本写不进时（例如数据卷满了）那笔先留在内存里，之后每次放行前先补写，补写不成就和读不出账本一样不再付费调用
（不让上限因为没记上而失效），写进去了自动恢复。超过月度上限后不再回答，念一句固定的话（到上限了、下个月一号恢复）；主人可以口头临时提高这个月的上限
（`HARO_MONTHLY_RAISE`，每次加多少），要过确认闸（登记过的声纹 + 有时效的确认）。听懂「要提高」靠一次很小的模型调用
（不带历史，只给提高上限和确认的工具，模型说的话不念），它不受额度管，按天限次数（`HARO_OVER_BUDGET_DAILY_CALLS`）。

### 连不上云端时（最低能力）

适配层把连不上（网络错误、等不到开始回答、超时、认证失败、限流、服务端出错；流开始之后才发来的错误事件按错误码同样归类）标成带类型的错误。这时不调用任何模型，
无论主人说什么，都念一句固定的话：连不上 + 现在的时间（中日英各一份）。连不上一次就熔断：之后马上告诉主人，
不用每句都等超时；30 秒后放一个请求去试，通了就恢复，下一轮由模型顺带交代刚才断过。

云端模型的单价来自公共的模型价格目录（`internal/pricecatalog`，默认 [models.dev](https://models.dev)，`HARO_MODEL_PRICES_*`）：
服务端启动时拉取（含缓存读写的单价、按上下文大小分的档），拉到的那一家存一份到 `HARO_MODEL_PRICES_CACHE`，
下次拉不到时用它；都没有、或目录里没有用到的模型时服务端起不来（费用上限要靠单价折算，不按 0 记）。
搜索积分的价格目录里没有，照旧在配置里写。

金额（`internal/money`）：数值存成「百万分之一个货币单位」的整数（micros，和 Google Ads 等接口同一种写法：一次调用常常只有零点零几美分，
用浮点累加有误差）+ 币种。汇率是欧洲央行每天公布的参考汇率（公开、不用密钥，`HARO_EXCHANGE_RATES_URL`）：设了费用上限才拉，
启动时拉一次、之后每 6 小时重拉；拉到的存一份到 `HARO_EXCHANGE_RATES_CACHE`，拉不到时用它，都没有时服务端起不来（不按猜的汇率判断上限）。
念给主人的金额按语言念币种：固定的话里写 `{名字:money}`，各语言的币种念法写在固定的话的文件里（`money` 一节，没列出的币种念「数值 代码」）。

## App 的接口（`internal/appserver`）

手机 App 和网页版（`web/`）用的 HTTP 接口，挂在服务端同一个端口的 `/api/v1/` 下（`appserver.PathPrefix`；公网上只放行这一段）。终端的语音连接、声纹登记在 `/v1/...` 下，
只在局域网：两段不能重叠（有测试守着），否则开放 App 接口时会把不鉴权的语音连接一起开出去。设了 `HARO_APP_PUBLIC_URL` 才开。

- **契约**：`api/openapi.yaml`（OpenAPI 3.0.3）。服务端的接口代码由 oapi-codegen 生成（`internal/appapi`，`go generate ./internal/appapi/`，⛔ 不手改），
  App 用 swift-openapi-generator 从同一份生成客户端；两边都不绕过契约私下约定字段。
- **入口的顺序**：请求体上限、按来源 IP 限流（登录、刷新、配对用严的一档）→ 按契约校验请求、对契约里要登录的接口验令牌
  （「哪些要登录」只写在契约里）→ 接口。出错一律 RFC 9457（`application/problem+json`）+ 稳定的错误码，说明按 `Accept-Language`（`texts.json`，每种语言一份）。
- **账号**（`internal/accounts`）：一个服务端多个账号。账号下面挂本人、访客两个「人」，对话、记忆、提醒等存储本来就按人存；
  每个请求先解析成账号，接口只用它的人取数据（隔离只在这一处做）。管理员是谁写在配置里（`HARO_ADMIN_SUBJECT`），启动时校正；
  管理员账号的人就是原来的声纹名（`HARO_ADMIN_PERSON`）和全家共用，现有数据不用改。新的 akasha 账号第一次登录时进「等管理员同意」，
  带着管理员发的邀请码（用一次、有时效，库里只存 SHA-256）登录的直接是正常账号；码不对、过期、用过的不建账号。
  管理员可以批准、停用（登录令牌全部作废、终端断开）、恢复、删除：删除是软删除（马上停用），30 天后 `accounts.Purger` 在每一处
  按人存数据的地方（对话和图片、背景记录池、记忆、提醒、日程、账目和原图、后台调查；组装时登记成一张清单，测试对照表结构防漏）
  删掉这个账号的两个人，全部删成了才删账号（资料、家、终端随它删）；有一处出错就留到第二天再试。花费账本不删（是费用记录）。
- **登录**：haro 后端是唯一的 akasha 客户端（机密客户端，`HARO_OIDC_*`），用 akasha 的 `pkg/oidcrp` 走授权码 + PKCE；
  App 用系统的网页登录框、网页版在浏览器里（带 `client=web`）打开 `/api/v1/auth/oidc/start`，登录完 haro 换成自己的令牌，跳回发起的一方：
  App 是固定的 `haro://auth/callback`，网页版是 `HARO_APP_WEB_URL` 的 `/auth/callback`（令牌在 fragment 里；失败时 `?oidc_error=`）。
  回跳只认这两个固定地址，回哪一方、邀请码编进 oidcrp 带过登录流程的 next，只放行自己编出来的写法。haro 的令牌是随机串、库里只存 SHA-256：访问令牌 1 小时，刷新令牌 90 天、用一次换新，
  用过的再来用时整串作废。
- **主人资料和家**（`internal/household`）：每个账号一份 —— 资料两段（全家的、本人的）、家的位置、节假日的国家，存在账号库里；
  大脑（资料）、天气（家在哪、每轮的「家大约在哪」）、节假日的提醒都按这一轮说话的人找到账号再取，App 的接口（`/me/profile`、`/me/home`、
  `/places`、`/countries`）在这里改，改了下一轮就生效。管理员账号在 App 里没设过的项用服务端的配置（`HARO_PROFILE_DIR`、`HARO_WEATHER_HOME`、
  `HARO_HOLIDAYS`），和没有账号时一样；别的账号没设时就是没有。终端上声纹认出的别的名字归管理员账号、只带「全家」那段（终端配对之前都归管理员）。
- **打字对话**（`internal/chat`）：一个人有语音那条对话（`brain.VoiceThread`，终端上说的都接在这里，App 里也能接着打字）和 App 里新建的几条（线程）。
  对话库按（人, 线程）存历史；轮次编号是每个人一套、所有线程共用，完整记录、向量、长期记忆的索引都还按（人, 编号）找；无痕线程的轮次不算向量。
  说一句 = 开一个打字会话（`brain.Session` 的 `TextOnly`：不开识别流、不合成语音、模型写的原样发出），打的字当成账号本人说的交进去
  （`Session.SubmitText`，等同声纹确认），同一套前台、工具、确认闸、额度；前台的规则按 `{{.Medium}}` 渲染成打字用的（能用 Markdown、不用情绪标记）。
  回答经 `POST /conversations/{id}/messages` 以 Server-Sent Events 流回去（手写挂载，验令牌、限流同其他接口）；App 断开后这一轮照样答完、存好；
  同一条对话同时只答一句。App 里新建的对话第一轮答完后，代理「起标题」在后台起标题（记在「对话摘要」的用途里）。
  图片：`POST /conversations/{id}/attachments`（请求体就是 JPEG / PNG / WebP，上限在 client-config）解码、长边缩到 1568 像素以内、统一存成 JPEG
  （`internal/attachments`，文件在 `HARO_ATTACHMENTS_DIR`，对话库记谁的、哪条对话的），说一句时带附件编号：图片只交给这一轮的模型，历史里记编号
  （和摄像头的画面一样，之后的轮次不重发）；`GET /attachments/{id}` 取回自己的。搜以前的对话（`/turns/search`）：按文字（子串）和按意思
  （长期记忆的向量）找到的合在一起，新的在前，删了的对话里的不算。
  背景记录池（`internal/backstory`）：对话闲下来后（跟着闲置压缩的时间），把新轮次（无痕、删了的对话除外）交给代理「背景记录池」并进去，
  每天整理一次；代理用 `save_backstory` 交回清单（不从文字里找），主人在 App 里改过的条目只读、后台不改不删；总长度 3000 字，
  接在主人资料后面放进系统提示词（内存里一份，每轮不读库），App 里能关掉。`/memory/backstory`、`/memory/notes`、`/research/tasks` 是查看、改的接口
  （调查只读，不标「已告诉」）。
  对话库迁移前自动备份到 `<库>.before-migrate`（`sqliteschema`：有待跑的迁移时先 `VACUUM INTO`）。
- **终端**：`/terminals`（列表、配对、改名、移除）、`/admin/terminals/pending`（管理员批准 / 拒绝新终端）；配对码输错按来源 IP 限流，
  同一个账号 10 分钟内输错 5 次也先停一停（`pairing_attempts_exceeded`）。终端的登记、设备凭证存在账号库（`terminals`、`terminal_registrations`）。
- **网页版跨域**：网页版在另一个域名，设了 `HARO_APP_WEB_URL` 才放行它这一个来源（`rs/cors`，方法从契约里取）；令牌在 `Authorization` 里，不用 cookie。
  跨域在限流外面一层，被限流、出错的回答也带跨域的头。
- **金额**：数值是十进制字符串 + 币种；汇总换成默认货币，带按币种的原始合计和换算提示。

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
  对话窗口结束去休眠（窗口默认 40 秒）、服务端的提示（例如后台调查查完了）、到点的提醒没能说出来（服务端的语音合成用不了）。

## 配置

全部来自环境变量（`internal/config/config.go` 里每一项都有说明）。通常写在一个 env 文件里，
systemd 用户服务从 `~/.config/haro/haro.env` 读取（`deploy/systemd/`）。凭证（云端模型、搜索服务的密钥）只放在环境变量里，没有默认值。
样例（只有占位值）：服务端 `deploy/server/server.env.example`，终端 `deploy/systemd/terminal.env.example`；
终端和服务端各读各的，写在同一个 env 文件里也互不影响（已删除的配置项也只查自己那边的）。
写错的值启动时就报错，不悄悄换成默认值：数值查范围（上限、单价不能是负数；时长、token、轮数要大于 0；
「0 = 不限 / 关掉」的项在 `config.go` 的注释里写明），配了的目录要读得到，默认性格要在性格档案里，声音档案里的情绪名要在情绪清单里。
有几项的默认值只写在用它的部件里，配置里不设就用它：工具时限、确认时效（`brain`）、调查的时限、搜索次数、token（`research.DefaultLimits`；
轮数是代理定义 `research` 的 `max_steps`）、
VAD 的门槛和静音时长（haro-asr 的 `asrserver.Timing`）。

**时区**：「今天」「这个月」（每日、每月的费用上限什么时候翻篇）、告诉模型的现在时间、记忆和提醒显示的时间都按服务端进程的本地时区算，
存储一律是 UTC。时区不单独做配置项，用标准的环境变量 `TZ`（例如 `Asia/Tokyo`；服务端程序里带着时区数据）：
不设时按系统的时区，容器里一般是 UTC。服务端、终端启动时都在日志里写出实际用的时区。

云端模型（`HARO_LLM_PROVIDER=cloud`）的接口格式由 `HARO_CLOUD_LLM_API` 选：

- `anthropic`（默认）：Claude API（官方 Go SDK `anthropic-sdk-go`）。`HARO_CLOUD_LLM_MODEL` 例如 `claude-haiku-5-5`；
  对话的思考关掉求快，回答力度用 `HARO_CLOUD_LLM_EFFORT`（聊天、摘要）/ `HARO_RESEARCH_EFFORT`（后台调查：查资料和审查，思考另见上文「后台调查」）；
  开着提示词缓存，缓存读写按模型价格目录里的单价分开记账。对话的缓存留 `HARO_CLOUD_LLM_CACHE_TTL`（默认 1 小时；
  写进 1 小时缓存的按输入价的 2 倍记：目录里只有 5 分钟的价，倍数由 `internal/pricecatalog` 按厂商补上），后台调查一步接一步地请求，用 5 分钟的。
  凭证：`HARO_ANTHROPIC_API_KEY`；不填时只认配齐的 Workload Identity Federation（`ANTHROPIC_FEDERATION_RULE_ID`、
  `ANTHROPIC_ORGANIZATION_ID`、`ANTHROPIC_IDENTITY_TOKEN_FILE`：在 Kubernetes 里用服务账号令牌换短期令牌，不放长期密钥，
  配法见 Anthropic 文档「Use WIF with Kubernetes」），没配齐就起不来。不走 SDK 的默认凭证链：
  环境里的 `ANTHROPIC_API_KEY`（开发机上是 Claude Code 的）不会被拿来用。
  服务端全进程只建一个 Claude 客户端（`llm.ClaudeAPI`），聊天、摘要、调查共用，HTTP 客户端也只用一个：
  SDK 按「客户端 × HTTP 客户端」各存一份令牌，而集群的服务账号令牌只能换一次，多一份就会被拒（401）。
- `openai`：任意 OpenAI 兼容的 `/chat/completions`（`HARO_CLOUD_LLM_URL`、`HARO_CLOUD_LLM_API_KEY`）。

**更强的模型**（可选，`HARO_STRONG_LLM_MODEL`；和云端模型同一个接口与凭证）：
用 `strong` 档的代理（强模型、看画面）用它。可以就是云端模型本身：它的思考写法 `HARO_STRONG_LLM_THINKING` 默认开着（`adaptive`），
力度 `HARO_STRONG_LLM_EFFORT`，每次输出上限 `HARO_STRONG_LLM_MAX_TOKENS`（思考也算在里面），所以比关着思考的前台想得深。什么时候交给它由前台的工具和规则定（看画面按配置、重答由前台模型认出主人在纠正后交出去），
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
- `research`：审查后台调查的结论（见上文「后台调查」）：云端模型搜索、读网页、写草稿，审查只看问题、草稿和草稿列出的来源的摘录，
  交回通过或疑点，疑点退回云端模型复查一次；不重写结论。审查出错时用草稿。不设 `research` 就不审查，草稿就是结论。
  审查默认由云端模型做（`HARO_RESEARCH_REVIEWER=base`），设成 `strong` 时由它做

接手时它出错或等太久（`HARO_STRONG_LLM_TIMEOUT`）由前台的模型接着回答。单价同样来自模型价格目录（目录里没有它就起不来）。
Claude 各模型「关掉思考」的写法不同：`HARO_CLOUD_LLM_THINKING` / `HARO_STRONG_LLM_THINKING`，Haiku 5.5 用 `disabled`，
Sonnet 5.5 只认 `between_tools`（两次工具调用之间会返回几句进度说明，对话里不回传；实测后续请求照样被接受）。

真实接口的冒烟测试（花钱，默认不跑）：`HARO_ANTHROPIC_API_KEY=… go test -tags integration -run AnthropicLive -v ./internal/llm/`；
看画面的专家：`go test -tags integration -run LiveExpert -v ./internal/vision/`（用法见该测试开头）；
认不认得出纠正：`go test -tags integration -run LiveAskStronger -v ./internal/brain/`。
看图小评测（花钱，默认不跑；样本放在仓库外）：`internal/llm/vision_eval_test.go`（`-tags eval`，先用 `HARO_EVAL_DRY_RUN=1` 看要花多少）。
搜索质量对比（花钱，默认不跑；题目和参考答案放在仓库外）：`internal/llm/search_eval_test.go`（`-tags eval`）：同一批题由现在的设置、
Anthropic 服务端搜索、Tavily 高级档各答一遍（真的会话，提示词照线上），先 `TestSearchEvalRun` 跑、填好参考答案再 `TestSearchEvalJudge` 判。
前台能力对比（花钱，默认不跑；题目放在仓库外）：`cmd/haro-server/front_eval_test.go`（`-tags eval`）：线上的前台（全套工具、
系统提示词、主人资料、默认性格、情况说明，第一句照线上叫醒）分别用 Claude 和本机 OpenAI 兼容服务上的模型（思考关 / 开）答同一批题，
先 `TestFrontEvalRun` 跑、再 `TestFrontEvalJudge` 判。
后台调查对比（花钱，默认不跑；题目放在仓库外）：`cmd/haro-server/research_eval_test.go`（`-tags eval`）：同一批题按几组设置各跑一次
真的后台调查（照线上装配），评审对照参考答案判。只考审查员：`cmd/haro-server/review_exam_test.go`（`-tags eval`）：同一批草稿
（原样的、埋了一处错的）交给几组审查员各审一次，走线上审查的同一段代码（`internal/research/review_exam.go`，只在 eval 标签下编译），
评审判埋的错抓没抓到。

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
  给机器读的用 `-version -json`（一行 JSON：program、version、os、arch、go），CI、安装脚本的测试按它比对。
- **终端和服务端握手时互报版本**（HTTP 头 `X-Haro-Version`）：版本不同时两边的日志都会提醒升级终端。
- **合并前的检查**（`.github/workflows/ci.yml`；日常开发在 `dev`，推 `dev` 和开 PR 时跑）：每一步做什么写在 `ci.yml` 开头的说明里
  （终端编哪几个平台只在它的 `platforms` 一处）。
- **合并到 `main` 就发版，几分钟内上线**（部署仓库开着 Argo CD 自动同步，不用人工同步）。发版的每一步只写在
  `.github/workflows/release.yml` 开头的说明里；只改文档（根目录的 `*.md`、`docs/`）的合并不发版，编进程序的文件（例如代理的提示词）改了照样发版。
  workflow 里的第三方 action 按提交 SHA 引用，Trivy 的镜像按 digest 引用。
- **模型的 Release**：原发布方随时可能删文件 ⇒ 模型第一次从原发布方下载、校验后，放进本仓库的 Release
  （`models-v1`，由 `.github/workflows/models.yml` 建：推 `models-v*` 标签，或在 Actions 页面手动运行）；之后构建只从这里下载。
- **依赖更新**：Dependabot 每周检查 Go 依赖、基础镜像（Dockerfile 里钉了 digest）、GitHub Actions，有新版就开 PR 到 `dev`（`.github/dependabot.yml`）；
  镜像构建时还会打上 Debian 的安全补丁。
- **安装终端**：`curl -fsSL https://github.com/bukahou/haro/releases/latest/download/install.sh | sh -s -- --server-url http://服务端地址:59130`
  （装到 `~/.local/bin/haro`，配置在 `~/.config/haro/haro.env`，Linux 装成 systemd 用户服务、macOS 装成 LaunchAgent；
  再跑一次就是升级；`--uninstall` 卸载，只删安装脚本装的那一份：按安装时记下的 sha256 认）。冒烟测试：`scripts/test-install.sh`。

## 安全说明

- **服务端和语音识别服务的接口没有鉴权**，只应该跑在你信任的网络里（例如家里的局域网，不要暴露到公网）。
- 声纹是生物特征：只存在服务端本机的数据目录里。
- 声音档案（`assets/voices/example.json` 是格式示例）请只用你有权使用的声音。
