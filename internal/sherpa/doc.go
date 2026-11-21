// Package sherpa 是 sherpa-onnx 的四个适配，都在 CPU 上跑：
//   - 语音识别：SenseVoice-Small（Recognizer，实现 asrserver.Transcriber）
//   - 说话检测：Silero VAD（NewSileroDetectorFactory，每个转写会话一个实例）
//   - 唤醒词：zipformer 关键词识别（NewKeywordSpotterFactory，模型共享，每个转写会话一个流）
//   - 声纹：CAM++ 说话人识别（SpeakerEmbedder，只算向量；比对和存取在纯 Go 的 voiceprint 包里）
//
// 模型文件由调用方给出（来自配置 config.ASRSettings：目录 + 文件名），这里不按文件名猜；VAD 的时长与门槛
// 来自 asrserver.Timing（经 asrserver.VoiceDetectorConfig）。sherpa-onnx 特有的东西（模型目录里的词表 tokens.txt、
// Silero 的窗口、VAD 内部的缓冲）只在这里出现。
//
// ⚠️ 用 cgo（sherpa-onnx 附带预编译的原生库），只允许 cmd/haro-asr 引用；服务端 haro-server 经 WebSocket / HTTP
// 连 haro-asr，自己保持纯 Go（CGO_ENABLED=0 编译）。
package sherpa
