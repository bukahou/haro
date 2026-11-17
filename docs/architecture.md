# haro 架构

haro 是一个家用语音助手：家里每个房间放一台**终端**（麦克风、音箱、可选摄像头），所有终端连到同一个**大脑**。
终端只管收音、放音、选设备；听懂、思考、说什么都在大脑里。

```
 终端 haro（每台机器一个）                          大脑 haro-brain（家里一个）
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
| `cmd/haro` | 终端。`haro -enroll <名字>` 登记声纹（录音经大脑登记） | cgo（音频库 miniaudio 随源码编译，不用装系统库）；摄像头需要 ffmpeg |
| `cmd/haro-brain` | 大脑 | 纯 Go |
| `cmd/haro-asr` | 本地语音识别服务，由大脑作为子进程启动；也可以单独运行，提供 OpenAI 兼容的转写接口 | cgo（sherpa-onnx） |

## 终端和大脑之间的协议

兼容 OpenAI Realtime API 的事件协议（WebSocket），只用其中的一个子集；haro 自己的扩展事件以 `haro.` 开头
（例如终端上报现在用的麦克风、音箱、摄像头）。见 `internal/realtimeproto`。

## 大脑（`internal/brain` 与周边）

大脑核心只依赖接口（`internal/brain/ports.go`），具体实现由 `cmd/haro-brain` 组装注入：

| 接口 | 实现 |
|---|---|
| `SpeechRecognizer` / `StreamingRecognizer` | `internal/speech`：本地 haro-asr、OpenAI 兼容的转写服务 |
| `ChatModel` | `internal/llm`：ollama（本地）、OpenAI 兼容接口（云端）；主备切换（`FallbackChatModel`） |
| `SpeechSynthesizer` | `internal/speech`：GPT-SoVITS（边合成边播放）、Piper |
| `Toolbox` | `internal/tools`：内置工具 + 第三方 MCP 服务 |
| `ConversationStore` | `internal/conversationstore`：每人一个文件 |
| `Briefer` | 每轮补充给模型的即时情况：现在几号几点、主人家大致在哪（城市级）、额度提醒、后台调查的任务板 |

- **对话跟着人走**：开了声纹时，大脑认出是谁在说话，用这个人自己的对话历史；换终端、断线重连、大脑重启都接得上。
  只保留最近几轮；更早的对话在后台压成一段摘要，放进系统提示词（`internal/brain/summary.go`）。
- **提示词的结构**：系统提示词（身份、性格、对话摘要）和历史保持不变，每轮会变的内容（时间、位置、谁在说话、任务板）
  附在这一轮主人的话前面——支持提示词缓存的模型按开头复用，本地模型也不用每轮从头处理；历史里只存主人的原话。
- **多台终端**：几台终端同时听到唤醒词时，大脑短暂等一下，选一台回答（离得近、正在播放的优先；`wake_arbiter.go`）。
- **性格**：`assets/personas/*.json`，可以用语音切换；回答里的情绪标记决定语音合成用哪段参考录音。

### 工具

自研工具内置在大脑里；**MCP 只用来接第三方服务**（本地子进程或远程 Streamable HTTP）。

| 工具 | 说明 |
|---|---|
| `get_current_time` | 现在的时间 |
| `save_note` / `search_notes` / `list_recent_notes` / `delete_note` | 记事本（SQLite）；删除前要口头确认 |
| `check_network` / `network_interfaces` | 网络诊断（查的是大脑所在机器的网络） |
| `get_weather` | 天气（Open-Meteo，不用密钥）；家的位置可以配坐标，或按公网 IP 推定 |
| `look_at_camera` | 看终端摄像头的画面（终端有摄像头时才提供） |
| `switch_persona` | 换性格（有两个以上性格时才提供） |
| `web_search` | 联网搜一次（Tavily 远程 MCP）；设了密钥才提供 |
| `start_research` / `research_status` / `cancel_research` | 后台调查（见下）；设了搜索密钥才提供 |

### 后台调查（`internal/research`）

需要多搜几次、读几篇资料的问题，大脑派一个后台任务，立刻回话，主人可以接着聊别的：

- 任务由程序强制上限：时间、轮数、搜索次数、token，先到先停；到上限后不再给工具，用已有资料写结论（标明没查完）。
- 任务和结论存在 SQLite 里；每轮对话的系统提示词里带着「任务板」，下次说话时先告诉主人结论。
- 调查必须先搜：模型想不搜就下结论时会被退回去。
- 查完时，大脑给主人最近说话的那台终端发一个提示（`haro.notice`），终端响一声「叮咚」（睡着也响）；
  主人叫醒后，大脑先说结论。大脑还不能主动开口说话。
- 调查用的模型可以单独配置（例如聊天用本地模型、调查用云端模型）。

### 费用（`internal/spending`）

大模型和搜索按量计费时：每次调用都记账（token、搜索积分，按配置的单价与汇率折成本地货币），
按天、按月封顶。超过月度上限后改用本地模型；没有本地模型时直接告诉主人额度用完了。

## 终端（`internal/terminal` 与 `internal/platform`）

分层，层与层之间只通过接口：

- **服务层** `internal/terminal`、`internal/assistant`：看守（连大脑 → 等麦克风 → 运行，出错就退回去重来）、
  选设备的规则（只看设备的接口类型：有别的麦克风时不用蓝牙麦克风，因为蓝牙耳机当麦克风会切到通话模式、音质下降）。
- **平台层** `internal/platform`：门面 `OpenLocalDevices`；`audio`（miniaudio）、`camera`（ffmpeg 取帧）、
  `osdiff`（各操作系统的差异：设备的接口类型怎么认、摄像头怎么找；按编译标签自动选，`make check-darwin` 在 Linux 上检查 macOS 版能否编译）。
- 提示音（`assets/tones`：内置的不含人声；可以用 `HARO_TONES_DIR` 换成自己录的）：大脑断开 / 恢复、
  对话窗口结束去休眠（窗口默认 40 秒）、大脑的提示（例如后台调查查完了）。

## 配置

全部来自环境变量（`internal/config/config.go` 里每一项都有说明）。通常写在一个 env 文件里，
systemd 用户服务从 `~/.config/haro/haro.env` 读取（`deploy/systemd/`）。凭证（云端模型、搜索服务的密钥）只放在环境变量里，没有默认值。

模型文件（识别、VAD、说完判断、唤醒词、声纹）不在仓库里，路径都由配置指定。

## 构建与运行

```
make build            # bin/haro、bin/haro-brain、bin/haro-asr
make test
make run              # 前台同时跑大脑和终端
make install-services # 装成 systemd 用户服务（跟着桌面会话启停：麦克风权限只给登录中的桌面会话）
make image            # 大脑镜像（deploy/brain/Dockerfile；模型用构建上下文 models 传进去）
```

## 安全说明

- **大脑和语音识别服务的接口没有鉴权**，只应该跑在你信任的网络里（例如家里的局域网，不要暴露到公网）。
- 声纹是生物特征：只存在大脑本机的数据目录里。
- 声音档案（`assets/voices/example.json` 是格式示例）请只用你有权使用的声音。
