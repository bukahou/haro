// haro-asr：本地语音识别服务（SenseVoice-Small + Silero VAD，sherpa-onnx，CPU）。提供
//   - GET  /v1/realtime?intent=transcription（WebSocket 流式转写，协议照 OpenAI Realtime 的转写会话）
//   - POST /v1/voiceprints/{name}（远程登记声纹）
//
// 单独运行（集群里和服务端是同一个 Pod 的两个容器；开发机上各一个 systemd 用户单元），服务端按 HARO_ASR_URL 连过来。
// 配置全部来自环境变量（config.ASRSettings）。
//
//	haro-asr [-version]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/sensevoice"
	"github.com/bukahou/haro/internal/smartturn"
	"github.com/bukahou/haro/internal/voiceprint"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Describe("haro-asr"))
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "haro-asr")
	settings, err := config.LoadASRSettings()
	if err == nil {
		err = run(settings, logger)
	}
	if err != nil {
		logger.Error("haro-asr 退出", "err", err)
		os.Exit(1)
	}
}

func run(settings config.ASRSettings, logger *slog.Logger) error {
	loadStarted := time.Now()
	recognizer, err := sensevoice.NewRecognizer(settings.ModelDir, settings.Threads, settings.Language)
	if err != nil {
		return err
	}
	defer recognizer.Close()
	logger.Info("模型已加载", "elapsed", time.Since(loadStarted).Round(time.Millisecond), "threads", settings.Threads)

	newDetector, err := sensevoice.NewSileroDetectorFactory(settings.VADModel)
	if err != nil {
		return err
	}
	var turnDetector asrserver.TurnDetector
	if settings.TurnModel != "" {
		detector, err := smartturn.NewDetector(settings.TurnModel, settings.OnnxruntimeLib)
		if err != nil {
			return err
		}
		defer detector.Close()
		turnDetector = detector
		logger.Info("说完判断（semantic_vad）已启用", "turn_model", settings.TurnModel)
	}

	kws := sensevoice.KeywordSpotterConfig{ModelDir: settings.KWSModelDir, KeywordsFile: settings.WakeWordsFile,
		Threshold: float32(settings.KWSThreshold), Score: float32(settings.KWSScore)}
	newKeywordSpotter, release, err := sensevoice.NewKeywordSpotterFactory(kws)
	if err != nil {
		return err
	}
	defer release()
	logger.Info("唤醒词已启用", "keywords_file", kws.KeywordsFile, "threshold", kws.Threshold)

	speakers, closeSpeakers, err := loadSpeakerCheck(settings, logger)
	if err != nil {
		return err
	}
	defer closeSpeakers()

	server := &http.Server{
		Addr: settings.Listen,
		Handler: asrserver.NewHandler(asrserver.Dependencies{Transcriber: recognizer,
			NewVoiceDetector: newDetector, TurnDetector: turnDetector, NewKeywordSpotter: newKeywordSpotter,
			Speakers: speakers, Logger: logger}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("开始监听", "addr", settings.Listen)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadSpeakerCheck 加载说话人识别模型和登记的声纹；没指定模型时返回 nil（不比对）。
// 返回的清理函数总是非 nil。
func loadSpeakerCheck(settings config.ASRSettings, logger *slog.Logger) (*asrserver.SpeakerCheck, func(), error) {
	if settings.SpeakerModel == "" {
		return nil, func() {}, nil
	}
	embedder, err := sensevoice.NewSpeakerEmbedder(settings.SpeakerModel, 2)
	if err != nil {
		return nil, nil, err
	}
	voiceprints, err := voiceprint.LoadAll(settings.VoiceprintsDir, embedder.ModelName())
	if err != nil {
		embedder.Close()
		return nil, nil, err
	}
	names := make([]string, 0, len(voiceprints))
	for _, v := range voiceprints {
		names = append(names, v.Name)
	}
	if len(voiceprints) == 0 {
		logger.Warn("声纹已启用，但还没有人登记（在终端用 haro -enroll <名字> 登记）：暂不过滤", "dir", settings.VoiceprintsDir)
	} else {
		logger.Info("声纹已启用", "speakers", names, "threshold", settings.SpeakerThreshold,
			"wake_threshold", settings.WakeSpeakerThreshold)
	}
	return &asrserver.SpeakerCheck{Embedder: embedder, Voiceprints: voiceprints, Threshold: settings.SpeakerThreshold,
			WakeThreshold: settings.WakeSpeakerThreshold, Dir: settings.VoiceprintsDir, Model: embedder.ModelName()},
		embedder.Close, nil
}
