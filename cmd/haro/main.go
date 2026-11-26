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
	"github.com/bukahou/haro/internal/terminaltools"
	"github.com/bukahou/haro/internal/trigger"
)

func main() {
	enrollName := flag.String("enroll", "", "登记声纹：在这台终端上照着念几句话，发给服务端登记，例如 -enroll 主人")
	enrollAppend := flag.Bool("append", false, "和 -enroll 一起用：补录（换了麦克风、换了位置时），加进原来的声纹")
	enrollSeconds := flag.Duration("seconds", 5*time.Second, "和 -enroll 一起用：每句录多久")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	versionJSON := flag.Bool("json", false, "和 -version 一起用：输出一行 JSON（program、version、os、arch、go），给安装脚本、CI 读")
	flag.Parse()
	if *showVersion {
		if err := buildinfo.WriteVersion(os.Stdout, "haro", *versionJSON); err != nil {
			fmt.Fprintln(os.Stderr, "打印版本号：", err)
			os.Exit(1)
		}
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
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
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

	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.ServiceTerminal, settings.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() { // 最多等 telemetry.ShutdownTimeout：采集器连不上时不拖住停止 / 重启
		if err := shutdownTelemetry(); err != nil {
			logger.Warn("退出时遥测没发出去", "err", err)
		}
	}()

	local, err := platform.OpenLocalDevices(logger)
	if err != nil {
		return err
	}
	defer local.Close()
	camera, err := openCamera(ctx, settings, local, logger)
	if err != nil {
		return err
	}
	sounds, err := tones.Load(settings.TonesDir) // 不设 = 内置的不含人声的提示音
	if err != nil {
		return err
	}
	keyboard := trigger.NewKeyboardTrigger(os.Stdin)
	options := assistantOptions(settings, sounds)
	logger.Info("haro 启动", "version", buildinfo.Version, "server", settings.ServerURL,
		"turn_detection", settings.TurnDetection, "barge_in", options.BargeIn, "camera_backend", settings.CameraBackend)
	zone := config.LocalTimeZone(time.Now())
	logger.Info("时区（日志里的时间按它；用环境变量 TZ 改）", "zone", zone.Name, "utc_offset", zone.Offset, "TZ", zone.TZ)
	for _, note := range settings.RemovedSettingNotes {
		logger.Warn(note)
	}

	// 看守：连服务端 → 等麦克风 → 运行；出什么错都退回去重来，只有关机信号才退出
	supervisor := terminal.NewSupervisor(terminal.SupervisorDependencies{
		Dial: func(ctx context.Context) (terminal.Connection, error) {
			return brainclient.Dial(ctx, brainclient.DialOptions{BaseURL: settings.ServerURL, InputFormat: local.MicrophoneFormat()}, logger)
		},
		Catalog:            local,
		MicrophoneOverride: settings.AudioSource,
		SpeakerOverride:    settings.AudioSink,
		CameraName:         cameraName(camera),
		Tools:              terminaltools.NewServer(camera, buildinfo.Version, logger),
		NewSession: func(conn terminal.Connection, devices terminal.SelectedDevices) terminal.Session {
			logger.Info("🎤 麦克风", "device", devices.Microphone.Name, "id", devices.Microphone.ID, "why", devices.Microphone.Reason)
			if devices.HasSpeaker {
				logger.Info("🔊 音箱", "device", devices.Speaker.Name, "why", devices.Speaker.Reason)
			} else {
				logger.Warn("没有音箱：照常听，回答只写日志", "why", devices.WhyNoSpeaker)
			}
			return assistant.NewAssistant(assistant.Dependencies{
				Manual:     keyboard,
				Microphone: local.OpenMicrophone(devices.Microphone.ID),
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
	return enrollment.Run(ctx, enrollment.Options{ServerURL: settings.ServerURL, Name: name,
		Append: appendMode, Seconds: seconds, Recorder: local.OpenRecorder(microphone.ID), In: os.Stdin, Out: os.Stdout,
		Client: &http.Client{Timeout: 60 * time.Second}})
}

// assistantOptions 由配置组装对话选项（系统提示词由服务端持有，终端不发）。
func assistantOptions(settings config.TerminalSettings, sounds tones.Tones) assistant.Options {
	return assistant.Options{
		TurnDetection: settings.TurnDetectionSettings(), // create_response 不填 = 服务端判定说完时自己开始回答
		EchoGuard:     settings.EchoGuard,
		WakeWord:      assistant.WakeWordOptions{IdleTimeout: settings.WakeIdle, SleepTone: sounds.Sleep},
		BargeIn:       settings.BargeIn,
		NoticeTone:    sounds.Notice,
		NoticeTones: map[string]realtimeproto.PCMAudio{ // 服务端那边听不见了：和服务端断开时同样的两声
			realtimeproto.NoticeTranscriptionLost:     sounds.Offline,
			realtimeproto.NoticeTranscriptionRestored: sounds.Online,
		},
	}
}

// openCamera 选摄像头并准备好（这时不打开，要看的时候才打开）。HARO_CAMERA_DEVICE=none 时不用摄像头，返回 nil。
// 列不出摄像头不退出；要的拍照方式这个系统上不支持时返回错误（配置写错了，启动失败）。
func openCamera(ctx context.Context, settings config.TerminalSettings, local *platform.LocalDevices,
	logger *slog.Logger) (terminal.Camera, error) {
	if settings.CameraDevice == terminal.CameraNone {
		logger.Info("📷 不用摄像头", "why", "配置关闭（HARO_CAMERA_DEVICE=none）")
		return nil, nil
	}
	cameras, err := local.Cameras(ctx)
	if err != nil {
		logger.Warn("列不出摄像头：当作现在没有", "err", err)
	}
	choice := terminal.ChooseCamera(cameras, settings.CameraDevice)
	if choice.ID == "" {
		logger.Info("📷 现在没有摄像头", "why", choice.Reason)
	} else {
		logger.Info("📷 摄像头", "device", choice.Name, "id", choice.ID, "why", choice.Reason)
	}
	return local.OpenCamera(choice, terminal.PickCameraFrom(local, settings.CameraDevice), terminal.CameraOptions{
		Backend:     cameraBackend(settings.CameraBackend),
		CaptureSize: terminal.FrameSize{Width: settings.CameraSize.Width, Height: settings.CameraSize.Height},
		OutputWidth: settings.FrameWidth,
	})
}

// cameraBackend 把配置值换成平台层的选项。
func cameraBackend(value string) terminal.CameraBackend {
	switch value {
	case config.CameraBackendMediaDevices:
		return terminal.CameraMediaDevices
	case config.CameraBackendFFmpeg:
		return terminal.CameraFFmpeg
	default:
		return terminal.CameraAuto
	}
}

// cameraName 让看守随时知道摄像头能不能用；没有摄像头时为 nil。
func cameraName(camera terminal.Camera) func() string {
	if camera == nil {
		return nil
	}
	return camera.CurrentName
}
