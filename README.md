# haro

家用语音助手：每个房间一台终端（麦克风、音箱、可选摄像头），连到同一个大脑。
叫一声唤醒词就能聊天、记事、查天气、联网搜索、派后台调查；按声纹认人，每个人有自己的对话。

- 终端：Go + miniaudio，Linux / macOS
- 大脑：Go；本地识别（sherpa-onnx：SenseVoice、Silero VAD、Smart Turn、唤醒词、CAM++ 声纹）；
  大模型用 ollama 或任意 OpenAI 兼容接口；语音合成用 GPT-SoVITS 或 Piper
- 终端 ↔ 大脑：兼容 OpenAI Realtime API 的 WebSocket 事件协议

架构、工具清单、配置与构建：见 [docs/architecture.md](docs/architecture.md)。

⚠️ 大脑与识别服务的接口没有鉴权，只在可信的局域网里运行。
