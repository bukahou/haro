// haro-asr：本地语音识别服务（SenseVoice-Small + Silero VAD，sherpa-onnx，CPU）。提供
//   - GET  /v1/realtime?intent=transcription（WebSocket 流式转写，协议照 OpenAI Realtime 的转写会话）
//   - POST /v1/voiceprints/{name}（远程登记声纹）
//
// 单独运行（集群里和服务端是同一个 Pod 的两个容器；开发机上各一个 systemd 用户单元），服务端按 HARO_ASR_URL 连过来。
// 配置全部来自环境变量（config.ASRSettings）。
//
//	haro-asr [-version]
//	haro-asr -probe livez|readyz   # 探一次自己（k8s 的 exec 探针：haro-asr 只监听本机，httpGet 探针连的 Pod IP 够不着）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/graceful"
	"github.com/bukahou/haro/internal/sherpa"
	"github.com/bukahou/haro/internal/smartturn"
	"github.com/bukahou/haro/internal/voiceprint"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	probe := flag.String("probe", "", "探一次自己的 livez 或 readyz 后退出：正常时退出码 0（k8s 的 exec 探针用）")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Describe("haro-asr"))
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "haro-asr")
	settings, err := config.LoadASRSettings()
	if err == nil && *probe != "" {
		err = probeSelf(settings.Listen, *probe)
	} else if err == nil {
		err = run(settings, logger)
	}
	if err != nil {
		logger.Error("haro-asr 退出", "err", err)
		os.Exit(1)
	}
}

// probeTimeout：探针一次最多等这么久（比 k8s 探针的 timeoutSeconds 短）。
const probeTimeout = 2 * time.Second

// probeSelf 请求一次本机上的 /livez 或 /readyz：返回 200 才算正常。
func probeSelf(listen, check string) error {
	if check != "livez" && check != "readyz" {
		return fmt.Errorf("-probe 只能是 livez 或 readyz，现在是 %q", check)
	}
	address := url.URL{Scheme: "http", Host: listen, Path: "/" + check}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s 返回 %d", address.String(), response.StatusCode)
	}
	return nil
}

// 关停：停止接新连接（最多等 shutdownTimeout 让普通请求做完）→ 结束进行中的转写会话 → 等它们退出（最多 sessionDrainTimeout）
// → 释放模型。两个期限加起来要比 k8s 的宽限期（30 秒）短。
const (
	shutdownTimeout     = 3 * time.Second
	sessionDrainTimeout = 5 * time.Second
)

func run(settings config.ASRSettings, logger *slog.Logger) error {
	// 模型是 cgo 里的内存：会话还在用时释放会段错误。所以只在会话都退出之后才释放（见 serve 的返回值）；
	// 会话没在期限内退出就不释放，进程退出时由系统回收。
	var models modelReleases
	releaseModels := true
	defer func() {
		if releaseModels {
			models.releaseAll()
		}
	}()

	loadStarted := time.Now()
	recognizer, err := sherpa.NewRecognizer(sherpa.RecognizerConfig{ModelDir: settings.ModelDir, ModelFile: settings.ModelFile,
		Threads: settings.Threads, Language: settings.Language})
	if err != nil {
		return err
	}
	models.add(func() { recognizer.Close() })
	logger.Info("模型已加载", "elapsed", time.Since(loadStarted).Round(time.Millisecond), "model_file", settings.ModelFile,
		"threads", settings.Threads)

	newDetector, err := sherpa.NewSileroDetectorFactory(settings.VADModel)
	if err != nil {
		return err
	}
	var turnDetector asrserver.TurnDetector
	if settings.TurnModel != "" {
		detector, err := smartturn.NewDetector(settings.TurnModel, settings.OnnxruntimeLib)
		if err != nil {
			return err
		}
		models.add(func() { detector.Close() })
		turnDetector = detector
		logger.Info("说完判断（semantic_vad）已启用", "turn_model", settings.TurnModel)
	}

	kws := sherpa.KeywordSpotterConfig{ModelDir: settings.KWSModelDir, EncoderFile: settings.KWSEncoderFile,
		DecoderFile: settings.KWSDecoderFile, JoinerFile: settings.KWSJoinerFile, KeywordsFile: settings.WakeWordsFile,
		Threshold: float32(settings.KWSThreshold), Score: float32(settings.KWSScore)}
	newKeywordSpotter, release, err := sherpa.NewKeywordSpotterFactory(kws)
	if err != nil {
		return err
	}
	models.add(release)
	wakePhrases, err := sherpa.KeywordPhrases(kws.KeywordsFile)
	if err != nil {
		return fmt.Errorf("读关键词文件里的唤醒词: %w", err)
	}
	logger.Info("唤醒词已启用", "keywords_file", kws.KeywordsFile, "phrases", wakePhrases, "threshold", kws.Threshold,
		"model_files", []string{kws.EncoderFile, kws.DecoderFile, kws.JoinerFile})

	speakers, closeSpeakers, err := loadSpeakerCheck(settings, logger)
	if err != nil {
		return err
	}
	models.add(closeSpeakers)

	handler := asrserver.NewHandler(asrserver.Dependencies{Transcriber: recognizer,
		NewVoiceDetector: newDetector, TurnDetector: turnDetector, NewKeywordSpotter: newKeywordSpotter,
		WakePhrases: wakePhrases, Speakers: speakers, Settings: capabilitySettings(), Logger: logger})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		return err
	}
	logger.Info("开始监听", "addr", settings.Listen)
	drained, err := graceful.Serve(ctx, listener, handler, graceful.Timing{Shutdown: shutdownTimeout, Drain: sessionDrainTimeout}, logger)
	releaseModels = drained
	if drained && err == nil {
		logger.Info("释放模型后退出")
	}
	return err
}

// capabilitySettings 是打开各项能力要设的环境变量（转写会话因为缺这项能力被拒时写进说明）。
func capabilitySettings() asrserver.CapabilitySettings {
	return asrserver.CapabilitySettings{VoiceDetector: settingName("VADModel"), TurnDetector: settingName("TurnModel"),
		KeywordSpotter: settingName("KWSModelDir") + "、" + settingName("WakeWordsFile")}
}

// settingName 是 config.ASRSettings 里这个字段对应的环境变量名，取自字段的 env 标签：名字只在 config 里写一处。
// 没有这个字段时为空（测试会发现）。
func settingName(field string) string {
	definition, ok := reflect.TypeFor[config.ASRSettings]().FieldByName(field)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(definition.Tag.Get("env"), ",")
	return name
}

// modelReleases 是要在退出时释放的模型（按加载的逆序释放）。
type modelReleases []func()

func (m *modelReleases) add(release func()) { *m = append(*m, release) }

func (m modelReleases) releaseAll() {
	for i := len(m) - 1; i >= 0; i-- {
		m[i]()
	}
}

// loadSpeakerCheck 加载说话人识别模型和登记的声纹；没指定模型时返回 nil（不比对）。
// 返回的清理函数总是非 nil。
func loadSpeakerCheck(settings config.ASRSettings, logger *slog.Logger) (*asrserver.SpeakerCheck, func(), error) {
	if settings.SpeakerModel == "" {
		return nil, func() {}, nil
	}
	embedder, err := sherpa.NewSpeakerEmbedder(settings.SpeakerModel, settings.SpeakerThreads)
	if err != nil {
		return nil, nil, err
	}
	voiceprints, err := loadVoiceprints(settings, embedder.ModelName(), logger)
	if err != nil {
		embedder.Close()
		return nil, nil, err
	}
	return &asrserver.SpeakerCheck{Embedder: embedder, Voiceprints: voiceprints, Threshold: settings.SpeakerThreshold,
			WakeThreshold: settings.WakeSpeakerThreshold, Dir: settings.VoiceprintsDir, Model: embedder.ModelName()},
		embedder.Close, nil
}

// loadVoiceprints 读登记的声纹（model 算的才用得上）并记下情况：用不上的（换了说话人识别模型的旧声纹等）逐个警告，
// 一个都用不上时写明是这个原因，不说成「还没有人登记」。
// 声纹目录不存在时建出来（第一次登记之前本来就没有，之后登记也存在这里）；建不了、读不了时报错，haro-asr 起不来：
// 配了目录却读不到多半是路径、挂载或权限不对，当成「没人登记」会悄悄关掉声纹过滤。
func loadVoiceprints(settings config.ASRSettings, model string, logger *slog.Logger) ([]voiceprint.Voiceprint, error) {
	dir := settings.VoiceprintsDir
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("建声纹目录: %w", err)
		}
		logger.Info("声纹目录还没有：建好了", "dir", dir)
	}
	voiceprints, skipped, err := voiceprint.LoadAll(dir, model)
	if err != nil {
		return nil, err
	}
	for _, unusable := range skipped {
		logger.Warn("这份声纹用不上（这个人会被当成没登记的人）：重新登记", "file", unusable.File, "name", unusable.Name,
			"model", unusable.Model, "err", unusable.Err)
	}
	names := make([]string, 0, len(voiceprints))
	for _, v := range voiceprints {
		names = append(names, v.Name)
	}
	switch {
	case len(voiceprints) > 0:
		logger.Info("声纹已启用", "speakers", names, "unusable", len(skipped), "threshold", settings.SpeakerThreshold,
			"wake_threshold", settings.WakeSpeakerThreshold)
	case len(skipped) > 0:
		logger.Warn("声纹已启用，但登记过的声纹都用不上（原因见上面每一份）：重新登记之前暂不过滤", "unusable", len(skipped), "dir", dir)
	default:
		logger.Warn("声纹已启用，但还没有人登记（在终端用 haro -enroll <名字> 登记）：暂不过滤", "dir", dir)
	}
	return voiceprints, nil
}
