// Package asrserver 提供 OpenAI 兼容的语音识别接口：
//   - POST /v1/audio/transcriptions：一问一答（整段音频）
//   - GET  /v1/realtime?intent=transcription：WebSocket 流式转写会话（server_vad），v2 免按键用
//
// 本包只管协议，识别引擎（Transcriber）与 VAD（VoiceDetector）都由外部注入（haro-asr 注入 SenseVoice / Silero）。
// 本包不依赖 cgo，可以单独测试。
package asrserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
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

const maxUploadBytes = 32 << 20 // 32MB：16kHz 单声道约 17 分钟，语音对话远用不到

// Dependencies 是 handler 需要的引擎。NewVoiceDetector 为 nil 时不提供流式转写（GET /v1/realtime 返回 503）；
// TurnDetector 为 nil 时不支持 semantic_vad。
type Dependencies struct {
	Transcriber       Transcriber
	NewVoiceDetector  VoiceDetectorFactory
	TurnDetector      TurnDetector
	NewKeywordSpotter KeywordSpotterFactory // 为 nil 时不支持唤醒词（haro.wake_word）
	Speakers          *SpeakerCheck         // 为 nil 或没有登记声纹时不比对（v3 H-3）
	Logger            *slog.Logger
}

// NewHandler 返回挂好路由的 HTTP handler。
func NewHandler(deps Dependencies) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		handleTranscription(w, r, deps.Transcriber, deps.Logger)
	})
	mux.HandleFunc("GET /v1/realtime", func(w http.ResponseWriter, r *http.Request) {
		handleRealtimeTranscription(w, r, deps)
	})
	mux.HandleFunc("POST /v1/voiceprints/{name}", func(w http.ResponseWriter, r *http.Request) {
		handleEnrollment(w, r, deps.Speakers) // 远程登记声纹（v4 C-3）
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func handleTranscription(w http.ResponseWriter, r *http.Request, transcriber Transcriber, logger *slog.Logger) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "无法解析 multipart 表单: "+err.Error())
		return
	}
	format := r.FormValue("response_format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "text" && format != "verbose_json" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("不支持的 response_format %q（支持 json / text / verbose_json）", format))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少 file 字段")
		return
	}
	defer file.Close()
	wav, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取上传文件失败: "+err.Error())
		return
	}
	audio, err := speech.DecodeWAV(wav)
	if err != nil {
		writeError(w, http.StatusBadRequest, "只支持 16 位 PCM 的 WAV: "+err.Error())
		return
	}

	started := time.Now()
	transcript, err := transcriber.Transcribe(r.Context(), audio)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeError(w, http.StatusInternalServerError, "识别失败: "+err.Error())
		return
	}
	logger.Info("识别完成", "audio", audio.Duration().Round(time.Millisecond),
		"elapsed", time.Since(started).Round(time.Millisecond), "text", transcript.Text)

	switch format {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, transcript.Text)
	case "verbose_json":
		writeJSON(w, http.StatusOK, map[string]any{
			"text": transcript.Text, "language": transcript.Language,
			"duration": audio.Duration().Seconds(),
			"emotion":  transcript.Emotion, "event": transcript.Event,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"text": transcript.Text})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError 按 OpenAI 的错误格式返回。
func writeError(w http.ResponseWriter, status int, message string) {
	errorType := "invalid_request_error"
	if status >= 500 {
		errorType = "server_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": errorType}})
}
