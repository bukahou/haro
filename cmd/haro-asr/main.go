// haro-asr：本地语音识别服务（SenseVoice-Small + Silero VAD，sherpa-onnx，CPU）。提供
//   - GET  /v1/realtime?intent=transcription（WebSocket 流式转写，协议照 OpenAI Realtime 的转写会话；需要 -vad-model）
//   - POST /v1/voiceprints/{name}（远程登记声纹）
//
// 平时由 haro-server 作为子进程托管；也可以单独运行，haro-server 用 HARO_ASR=openai + HARO_ASR_URL 连过来。
//
//	haro-asr -model-dir <SenseVoice 模型目录> [-vad-model silero_vad.onnx] [-turn-model smart-turn-v3.2-cpu.onnx]
//	         [-listen 127.0.0.1:59126] [-threads 4]
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
	"github.com/bukahou/haro/internal/sensevoice"
	"github.com/bukahou/haro/internal/smartturn"
	"github.com/bukahou/haro/internal/voiceprint"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:59126", "监听地址（默认只对本机开放）")
	modelDir := flag.String("model-dir", "", "SenseVoice 模型目录（含 model.int8.onnx 与 tokens.txt）")
	threads := flag.Int("threads", 4, "推理线程数（实测 4 线程每句 24–35ms，再多收益很小）")
	language := flag.String("language", "auto", "语种：auto / zh / en / ja / ko / yue")
	vadModel := flag.String("vad-model", "", "Silero VAD 模型（silero_vad.onnx）；不指定则不提供流式转写")
	turnModel := flag.String("turn-model", "", "Smart Turn v3 模型（smart-turn-v3.2-cpu.onnx）；不指定则不支持 semantic_vad")
	onnxruntimeLib := flag.String("onnxruntime-lib", "libonnxruntime.so", "onnxruntime 动态库（默认用随 sherpa-onnx 加载的那份）")
	kwsModelDir := flag.String("kws-model-dir", "", "唤醒词模型目录（sherpa-onnx KWS）；不指定则不支持唤醒词")
	keywordsFile := flag.String("keywords-file", "", "关键词文件（每行：拼音单元 … [:加分] [#门槛] @显示名）")
	kwsThreshold := flag.Float64("kws-threshold", 0.25, "唤醒词默认触发门槛（0–1，越低越容易触发）")
	kwsScore := flag.Float64("kws-score", 1.0, "唤醒词默认加分")
	speakerModel := flag.String("speaker-model", "", "说话人识别模型（sherpa-onnx CAM++）；不指定则不比对声纹（v3 H-3）")
	voiceprintsDir := flag.String("voiceprints-dir", "", "登记的声纹目录（终端 haro -enroll 登记生成）")
	speakerThreshold := flag.Float64("speaker-threshold", 0.5, "声纹相似度达到这个值才算认出来（0–1）")
	wakeSpeakerThreshold := flag.Float64("wake-speaker-threshold", 0.2,
		"唤醒词的声纹达到这个值就能唤醒（唤醒词很短，算不稳；没到 -speaker-threshold 的，接着说的话还要再比对）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Describe("haro-asr"))
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "haro-asr")
	if err := run(options{listen: *listen, modelDir: *modelDir, vadModel: *vadModel, turnModel: *turnModel,
		onnxruntimeLib: *onnxruntimeLib, threads: *threads, language: *language,
		kws: sensevoice.KeywordSpotterConfig{ModelDir: *kwsModelDir, KeywordsFile: *keywordsFile,
			Threshold: float32(*kwsThreshold), Score: float32(*kwsScore)},
		speakerModel: *speakerModel, voiceprintsDir: *voiceprintsDir, speakerThreshold: *speakerThreshold,
		wakeSpeakerThreshold: *wakeSpeakerThreshold}, logger); err != nil {
		logger.Error("haro-asr 退出", "err", err)
		os.Exit(1)
	}
}

type options struct {
	listen, modelDir, vadModel, turnModel, onnxruntimeLib, language string
	threads                                                         int
	kws                                                             sensevoice.KeywordSpotterConfig
	speakerModel, voiceprintsDir                                    string
	speakerThreshold, wakeSpeakerThreshold                          float64
}

func run(opts options, logger *slog.Logger) error {
	listen, modelDir, vadModel, threads, language := opts.listen, opts.modelDir, opts.vadModel, opts.threads, opts.language
	if modelDir == "" {
		return errors.New("必须指定 -model-dir")
	}
	loadStarted := time.Now()
	recognizer, err := sensevoice.NewRecognizer(modelDir, threads, language)
	if err != nil {
		return err
	}
	defer recognizer.Close()
	logger.Info("模型已加载", "elapsed", time.Since(loadStarted).Round(time.Millisecond), "threads", threads)

	var newDetector asrserver.VoiceDetectorFactory
	if vadModel != "" {
		if newDetector, err = sensevoice.NewSileroDetectorFactory(vadModel); err != nil {
			return err
		}
		logger.Info("流式转写已启用", "vad_model", vadModel)
	}
	var turnDetector asrserver.TurnDetector
	if opts.turnModel != "" {
		detector, err := smartturn.NewDetector(opts.turnModel, opts.onnxruntimeLib)
		if err != nil {
			return err
		}
		defer detector.Close()
		turnDetector = detector
		logger.Info("说完判断（semantic_vad）已启用", "turn_model", opts.turnModel)
	}

	var newKeywordSpotter asrserver.KeywordSpotterFactory
	if opts.kws.ModelDir != "" && opts.kws.KeywordsFile != "" {
		factory, release, err := sensevoice.NewKeywordSpotterFactory(opts.kws)
		if err != nil {
			return err
		}
		defer release()
		newKeywordSpotter = factory
		logger.Info("唤醒词已启用", "keywords_file", opts.kws.KeywordsFile, "threshold", opts.kws.Threshold)
	}

	speakers, closeSpeakers, err := loadSpeakerCheck(opts, logger)
	if err != nil {
		return err
	}
	defer closeSpeakers()

	server := &http.Server{
		Addr: listen,
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
	logger.Info("开始监听", "addr", listen)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadSpeakerCheck 加载说话人识别模型和登记的声纹；没指定模型时返回 nil（不比对）。
// 返回的清理函数总是非 nil。
func loadSpeakerCheck(opts options, logger *slog.Logger) (*asrserver.SpeakerCheck, func(), error) {
	if opts.speakerModel == "" {
		return nil, func() {}, nil
	}
	embedder, err := sensevoice.NewSpeakerEmbedder(opts.speakerModel, 2)
	if err != nil {
		return nil, nil, err
	}
	voiceprints, err := voiceprint.LoadAll(opts.voiceprintsDir, embedder.ModelName())
	if err != nil {
		embedder.Close()
		return nil, nil, err
	}
	names := make([]string, 0, len(voiceprints))
	for _, v := range voiceprints {
		names = append(names, v.Name)
	}
	if len(voiceprints) == 0 {
		logger.Warn("声纹已启用，但还没有人登记（在终端用 haro -enroll <名字> 登记）：暂不过滤", "dir", opts.voiceprintsDir)
	} else {
		logger.Info("声纹已启用", "speakers", names, "threshold", opts.speakerThreshold,
			"wake_threshold", opts.wakeSpeakerThreshold)
	}
	return &asrserver.SpeakerCheck{Embedder: embedder, Voiceprints: voiceprints, Threshold: opts.speakerThreshold,
			WakeThreshold: opts.wakeSpeakerThreshold, Dir: opts.voiceprintsDir, Model: embedder.ModelName()},
		embedder.Close, nil
}
