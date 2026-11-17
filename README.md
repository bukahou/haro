# haro

> **haro** is a home voice assistant written in Go: one small **terminal** per room (microphone, speaker, optional camera)
> and a single **brain** that does local speech recognition (sherpa-onnx), talks to any OpenAI-compatible LLM,
> calls tools (notes, weather, web search, background research) and speaks back (GPT-SoVITS or Piper).
> Terminals and the brain speak an OpenAI Realtime API–compatible WebSocket protocol.
> Documentation is in Chinese; see [docs/architecture.md](docs/architecture.md).
>
> ⚠️ The brain and speech-recognition endpoints have **no authentication** — run them only on a trusted LAN.

家用语音助手：每个房间一台终端（麦克风、音箱、可选摄像头），连到同一个大脑。
叫一声唤醒词就能聊天、记事、查天气、联网搜索、派后台调查；按声纹认人，每个人有自己的对话。

- 终端：Go + miniaudio，Linux / macOS
- 大脑：Go；本地识别（sherpa-onnx：SenseVoice、Silero VAD、Smart Turn、唤醒词、CAM++ 声纹）；
  大模型用 ollama 或任意 OpenAI 兼容接口；语音合成用 GPT-SoVITS 或 Piper
- 终端 ↔ 大脑：兼容 OpenAI Realtime API 的 WebSocket 事件协议

架构、工具清单、配置与构建：见 [docs/architecture.md](docs/architecture.md)。

⚠️ 大脑与识别服务的接口没有鉴权，只在可信的局域网里运行。

## License

[Apache License 2.0](LICENSE)。安全问题请看 [SECURITY.md](SECURITY.md)。
