# haro

> **haro** is a home voice assistant written in Go: one small **terminal** per room (microphone, speaker, optional camera)
> and a single **server** that does local speech recognition (sherpa-onnx), talks to any OpenAI-compatible LLM,
> calls tools (notes, weather, web search, background research) and speaks back (GPT-SoVITS or Piper).
> Terminals and the server speak an OpenAI Realtime API–compatible WebSocket protocol.
> Documentation is in Chinese; see [docs/architecture.md](docs/architecture.md).
>
> Install a terminal (Linux x86_64 / arm64; macOS builds are paused; user-level, no root):
> `curl -fsSL https://github.com/bukahou/haro/releases/latest/download/install.sh | sh -s -- --server-url http://<server>:59130`.
> The server ships as the Docker image `bukahou/haro-server` (models included; see [deploy/server/MODELS.md](deploy/server/MODELS.md)).
>
> ⚠️ The server and speech-recognition endpoints have **no authentication** — run them only on a trusted LAN.

家用语音助手：每个房间一台终端（麦克风、音箱、可选摄像头），连到同一个服务端。
叫一声唤醒词就能聊天、记事、查天气、联网搜索、派后台调查；按声纹认人，每个人有自己的对话。

- 终端：Go + miniaudio，Linux / macOS
- 服务端：Go；本地识别（sherpa-onnx：SenseVoice、Silero VAD、Smart Turn、唤醒词、CAM++ 声纹）；
  大模型用 ollama、Claude API（官方 Go SDK）或任意 OpenAI 兼容接口；语音合成用 GPT-SoVITS 或 Piper
- 终端 ↔ 服务端：兼容 OpenAI Realtime API 的 WebSocket 事件协议

架构、工具清单、配置与构建：见 [docs/architecture.md](docs/architecture.md)。

## 安装

终端（Linux x86_64 / arm64；Mac 版暂停发布；装在自己的用户目录，不需要 root；再跑一次就是升级）：

```sh
curl -fsSL https://github.com/bukahou/haro/releases/latest/download/install.sh | sh -s -- --server-url http://服务端地址:59130
```

选项（`--version`、`--no-service`、`--uninstall`）见 [scripts/install.sh](scripts/install.sh)。
服务端是 Docker 镜像 `bukahou/haro-server`（模型已经在镜像里：[deploy/server/MODELS.md](deploy/server/MODELS.md)）。

⚠️ 服务端与识别服务的接口没有鉴权，只在可信的局域网里运行。

## License

[Apache License 2.0](LICENSE)。安全问题请看 [SECURITY.md](SECURITY.md)。
