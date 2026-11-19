// haro：终端（只管硬件：麦克风、摄像头、音箱、对话窗口）。
// 「听懂、思考、说什么」都在服务端 haro-server 里，两者经 WebSocket 传 Realtime 事件（架构见 docs/architecture.md）。
// 这里只做组装（组装点）：读配置 → 打开平台层（internal/platform）→ 注入看守（internal/terminal）一直运行。
// 只有这里认识具体实现；看守与对话逻辑只认接口。
// 服务端断了、设备没了都不退出，由看守重连、等设备；只有关机信号才退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bukahou/haro/assets/tones"
	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/brainclient"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/enrollment"
	"github.com/bukahou/haro/internal/platform"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/telemetry"
	"github.com/bukahou/haro/internal/terminal"
	"github.com/bukahou/haro/internal/trigger"
)

// microphoneFormat 是麦克风上传给服务端的格式（平台层固定为 16 kHz 单声道；声明在 session.update 里）。
var microphoneFormat = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}

func main() {
	enrollName := flag.String("enroll", "", "登记声纹：在这台终端上录 3 句话，发给服务端登记，例如 -enroll 主人")
	enrollAppend := flag.Bool("append", false, "和 -enroll 一起用：补录（换了麦克风、换了位置时），加进原来的声纹")
	enrollSeconds := flag.Duration("seconds", 5*time.Second, "和 -enroll 一起用：每句录多久")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Describe("haro"))
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if *enrollName != "" {
		if err := enroll(logger, *enrollName, *enrollAppend, *enrollSeconds); err != nil {
			fmt.Fprintln(os.Stderr, "登记失败：", err)
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		logger.Error("haro 退出", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.LoadTerminalSettings()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.SetupTelemetry(ctx, settings.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer shutdownTelemetry(context.Background())

	local, err := platform.OpenLocalDevices(logger)
	if err != nil {
		return err
	}
	defer local.Close()
	camera := openCamera(ctx, settings, local, logger)
	sounds, err := tones.Load(settings.TonesDir) // 不设 = 内置的不含人声的提示音
	if err != nil {
		return err
	}
	keyboard := trigger.NewKeyboardTrigger(os.Stdin)
	options := assistantOptions(settings, sounds)
	logger.Info("haro 启动", "version", buildinfo.Version, "server", settings.ServerURL,
		"turn_detection", settings.TurnDetection, "barge_in", options.BargeIn, "camera_mode", settings.CameraMode)
	for _, note := range settings.RemovedSettingNotes {
		logger.Warn(note)
	}

	// 看守：连服务端 → 等麦克风 → 运行；出什么错都退回去重来，只有关机信号才退出
	supervisor := terminal.NewSupervisor(terminal.Parts{
		Dial: func(ctx context.Context) (terminal.Connection, error) {
			return brainclient.Dial(ctx, settings.ServerURL, microphoneFormat, 0, logger)
		},
		Catalog:            local,
		MicrophoneOverride: settings.AudioSource,
		SpeakerOverride:    settings.AudioSink,
		CameraName:         cameraName(camera),
		NewSession: func(conn terminal.Connection, devices terminal.Devices) terminal.Session {
			logger.Info("🎤 麦克风", "device", devices.Microphone.Name, "why", devices.Microphone.Reason)
			if devices.HasSpeaker {
				logger.Info("🔊 音箱", "device", devices.Speaker.Name, "why", devices.Speaker.Reason)
			} else {
				logger.Warn("没有音箱：照常听，回答只写日志")
			}
			return assistant.NewAssistant(assistant.Components{
				Manual:     keyboard,
				Microphone: local.OpenMicrophone(devices.Microphone.ID),
				Camera:     camera, // nil = 没有摄像头（会告诉服务端）
				Speaker:    local.OpenSpeaker(devices.Speaker.ID),
				Brain:      conn,
			}, options, logger)
		},
		ToneSink: local.OpenSpeaker(settings.AudioSink),
		Offline:  sounds.Offline,
		Online:   sounds.Online,
	}, terminal.DefaultTiming, logger)
	return supervisor.Run(ctx)
}

// enroll 是 haro -enroll：用这台终端的麦克风录音，发给服务端登记声纹（服务端要在线）。
func enroll(logger *slog.Logger, name string, appendMode bool, seconds time.Duration) error {
	settings, err := config.LoadTerminalSettings()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	local, err := platform.OpenLocalDevices(logger)
	if err != nil {
		return err
	}
	defer local.Close()
	microphones, err := local.Microphones()
	if err != nil {
		return err
	}
	microphone, err := terminal.ChooseMicrophone(microphones, settings.AudioSource)
	if err != nil {
		return err
	}
	fmt.Printf("🎤 麦克风：%s（%s）\n服务端：%s\n", microphone.Name, microphone.Reason, settings.ServerURL)
	return enrollment.Run(ctx, enrollment.Options{ServerURL: settings.ServerURL, Name: name, Append: appendMode,
		Seconds: seconds, Recorder: local.OpenRecorder(microphone.ID), In: os.Stdin, Out: os.Stdout,
		Client: &http.Client{Timeout: 60 * time.Second}})
}

// assistantOptions 由配置组装对话选项（系统提示词由服务端持有，终端不发）。
func assistantOptions(settings config.Settings, sounds tones.Tones) assistant.Options {
	createResponse := false // 由终端带上「说完那一刻」的画面后再请求回答
	return assistant.Options{
		TurnDetection: realtimeproto.TurnDetection{
			Type: settings.TurnDetection, Eagerness: settings.TurnEagerness,
			Threshold: settings.VADThreshold, CreateResponse: &createResponse,
			SilenceDurationMs: int(settings.VADSilence.Milliseconds()),
			PrefixPaddingMs:   int(settings.VADPrefix.Milliseconds()),
		},
		EchoGuard:  settings.EchoGuard,
		WakeWord:   assistant.WakeWordOptions{Phrase: settings.WakePhrase, IdleTimeout: settings.WakeIdle, SleepTone: sounds.Sleep},
		BargeIn:    settings.BargeIn,
		NoticeTone: sounds.Notice,
	}
}

// openCamera 选摄像头并打开；启动时没有摄像头就不用（之后插上要重启终端），返回 nil。
// 启动时有、之后被拔掉或换了设备号，由平台层按同一套规则重新找。列不出摄像头也不退出。
func openCamera(ctx context.Context, settings config.Settings, local *platform.LocalDevices,
	logger *slog.Logger) terminal.Camera {
	cameras, err := local.Cameras(ctx)
	if err != nil {
		logger.Warn("列不出摄像头：当作没有", "err", err)
	}
	choice := terminal.ChooseCamera(cameras, settings.CameraDevice)
	if choice.ID == "" {
		logger.Info("📷 不用摄像头", "why", choice.Reason)
		return nil
	}
	logger.Info("📷 摄像头", "device", choice.Name, "id", choice.ID, "why", choice.Reason)
	return local.OpenCamera(ctx, choice, terminal.PickCameraFrom(local, settings.CameraDevice), terminal.CameraOptions{
		Continuous: settings.CameraMode == config.CameraContinuous, CaptureSize: settings.CameraSize,
		OutputWidth: settings.FrameWidth, FPS: settings.CameraFPS, MaxAge: settings.FrameMaxAge,
	})
}

// cameraName 让看守随时知道摄像头能不能用；没有摄像头时为 nil。
func cameraName(camera terminal.Camera) func() string {
	if camera == nil {
		return nil
	}
	return camera.CurrentName
}
