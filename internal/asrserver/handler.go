// Package asrserver 提供语音识别接口：
//   - GET  /v1/realtime?intent=transcription：WebSocket 流式转写会话（协议照 OpenAI Realtime 的转写会话）
//   - POST /v1/voiceprints/{name}：远程登记声纹
//
// 本包只管协议，识别引擎（Transcriber）与 VAD（VoiceDetector）都由外部注入（haro-asr 注入 SenseVoice / Silero）。
// 本包不依赖 cgo，可以单独测试。
package asrserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/bukahou/haro/internal/readiness"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Transcriber 是识别引擎。
type Transcriber interface {
	Transcribe(ctx context.Context, audio realtimeproto.PCMAudio) (Transcript, error)
}

// Transcript 是一次识别的结果。Emotion / Event 是 SenseVoice 特有的，其他引擎可留空。
type Transcript struct {
	Text     string
	Language string // 例如 "zh"
	Emotion  string // 例如 "NEUTRAL"、"HAPPY"
	Event    string // 例如 "Speech"、"Laughter"
}

// Dependencies 是 handler 需要的引擎。NewVoiceDetector 为 nil 时不提供流式转写（GET /v1/realtime 返回 503）；
// TurnDetector 为 nil 时不支持 semantic_vad。
type Dependencies struct {
	Transcriber       Transcriber
	NewVoiceDetector  VoiceDetectorFactory
	TurnDetector      TurnDetector
	NewKeywordSpotter KeywordSpotterFactory // 为 nil 时不支持唤醒词（haro.wake_word）
	Speakers          *SpeakerCheck         // 为 nil 或没有登记声纹时不比对
	Timing            Timing                // 时长与门槛；零值 = DefaultTiming
	Settings          CapabilitySettings    // 打开各项能力要设的配置项（拒绝时写进说明）；可以为空
	Logger            *slog.Logger
}

// CapabilitySettings 是打开每项能力要设的配置项：没有这项能力而被拒时写进说明，告诉对方该设什么。
// haro-asr 按它的配置项名字填（名字只在 config 里定义一处）；为空时说明里就不提。
type CapabilitySettings struct {
	VoiceDetector  string // 流式转写要的 VAD 模型
	TurnDetector   string // semantic_vad 要的说完判断模型
	KeywordSpotter string // 唤醒词要的模型和关键词文件
}

// missing 说明缺了哪项能力；知道由哪个配置项打开时一并写上。
func missing(capability, setting string) string {
	if setting == "" {
		return "没有" + capability
	}
	return "没有" + capability + "（要设 " + setting + "）"
}

// NewHandler 返回挂好路由的 HTTP handler。
func NewHandler(deps Dependencies) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+realtimeproto.TranscriptionPath, func(w http.ResponseWriter, r *http.Request) {
		handleRealtimeTranscription(w, r, deps)
	})
	mux.HandleFunc(realtimeproto.VoiceprintRoute, func(w http.ResponseWriter, r *http.Request) {
		handleEnrollment(w, r, deps.Speakers) // 远程登记声纹
	})
	// 存活只看进程能不能响应；就绪要在期限内真的识别完一小段静音（readiness.go）
	mux.HandleFunc("GET /livez", readiness.Alive)
	mux.Handle("GET /readyz", newRecognizerReadiness(deps.Transcriber, deps.Logger))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError 按 OpenAI 的错误格式返回。
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, realtimeproto.NewHTTPError(status, "", message))
}
