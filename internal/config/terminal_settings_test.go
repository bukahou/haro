package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

// terminalSettingsFrom 按 values 解析终端配置（没写的用默认值），不读跑测试那台机器的环境变量。
func terminalSettingsFrom(t *testing.T, values map[string]string) TerminalSettings {
	t.Helper()
	var settings TerminalSettings
	if err := env.ParseWithOptions(&settings, env.Options{Environment: values}); err != nil {
		t.Fatal(err)
	}
	return settings
}

// HARO_SERVER_URL 启动时就查：要是 http(s)://主机[:端口]；写成 ws://、不带协议、没有主机名时起不来，报错里提到这个配置项。
func TestServerURLIsValidated(t *testing.T) {
	settings := terminalSettingsFrom(t, map[string]string{"HARO_TURN_DETECTION": "server_vad"})
	for _, good := range []string{"http://127.0.0.1:59130", "https://haro.example.com", "HTTP://127.0.0.1:59130/"} {
		settings.ServerURL = good
		if err := settings.validateTerminal(); err != nil {
			t.Errorf("%s 应该可以：%v", good, err)
		}
	}
	for _, bad := range []string{"", "ws://127.0.0.1:59130", "127.0.0.1:59130", "haro.example.com", "http://"} {
		settings.ServerURL = bad
		if err := settings.validateTerminal(); err == nil || !strings.Contains(err.Error(), "HARO_SERVER_URL") {
			t.Errorf("%q 应起不来、报错里提到 HARO_SERVER_URL：%v", bad, err)
		}
	}
}

// 终端的数值和目录：说完判断的取值按 realtimeproto 的定义查（和 haro-asr 同一份）；对话窗口、画面宽度要大于 0；
// 播完后的静音不能是负数；提示音目录配了就必须读得到。每改坏一项都起不来，报错写明是哪个变量。
func TestTerminalSettingsAreValidated(t *testing.T) {
	base := terminalSettingsFrom(t, nil)
	if err := base.validateTerminal(); err != nil {
		t.Fatalf("默认配置应通过：%v", err)
	}
	notDir := filepath.Join(t.TempDir(), "notice.wav")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*TerminalSettings){
		"HARO_TURN_EAGERNESS": func(s *TerminalSettings) { s.TurnEagerness = "fast" },
		"HARO_VAD_THRESHOLD":  func(s *TerminalSettings) { s.VADThreshold = 2 },
		"HARO_VAD_SILENCE":    func(s *TerminalSettings) { s.VADSilence = -time.Millisecond },
		"HARO_WAKE_IDLE":      func(s *TerminalSettings) { s.WakeIdle = 0 },
		"HARO_FRAME_WIDTH":    func(s *TerminalSettings) { s.FrameWidth = 0 },
		"HARO_ECHO_GUARD":     func(s *TerminalSettings) { s.EchoGuard = -time.Millisecond },
		"HARO_TONES_DIR":      func(s *TerminalSettings) { s.TonesDir = filepath.Join(t.TempDir(), "没有这个目录") },
	} {
		settings := base
		change(&settings)
		if err := settings.validateTerminal(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s 写错应起不来并写明：%v", name, err)
		}
	}
	settings := base
	settings.TonesDir = notDir
	if err := settings.validateTerminal(); err == nil || !strings.Contains(err.Error(), "HARO_TONES_DIR") {
		t.Errorf("提示音目录是个文件应起不来：%v", err)
	}
	settings = base
	settings.TonesDir, settings.EchoGuard = t.TempDir(), 0
	if err := settings.validateTerminal(); err != nil {
		t.Errorf("读得到的提示音目录、播完不等（0）都可以：%v", err)
	}
}

// 遥测的地址（终端和服务端都有）：不设 = 不导出；设了就要是 http(s)://主机，写错时两边都起不来。
func TestOTLPEndpointIsValidated(t *testing.T) {
	terminal, server := terminalSettingsFrom(t, nil), localServerSettings(t)
	for _, good := range []string{"", "http://127.0.0.1:4318", "https://otel.example.com"} {
		terminal.OTLPEndpoint, server.OTLPEndpoint = good, good
		if err := terminal.validateTerminal(); err != nil {
			t.Errorf("终端：%q 应该可以：%v", good, err)
		}
		if err := server.validateBrain(); err != nil {
			t.Errorf("服务端：%q 应该可以：%v", good, err)
		}
	}
	for _, bad := range []string{"127.0.0.1:4318", "grpc://127.0.0.1:4317", "http://"} {
		terminal.OTLPEndpoint, server.OTLPEndpoint = bad, bad
		if err := terminal.validateTerminal(); err == nil || !strings.Contains(err.Error(), "HARO_OTLP_ENDPOINT") {
			t.Errorf("终端：%q 应起不来并写明 HARO_OTLP_ENDPOINT：%v", bad, err)
		}
		if err := server.validateBrain(); err == nil || !strings.Contains(err.Error(), "HARO_OTLP_ENDPOINT") {
			t.Errorf("服务端：%q 应起不来并写明 HARO_OTLP_ENDPOINT：%v", bad, err)
		}
	}
}

// 语音概率门槛和静音时长不设时不交给 haro-asr（默认值只在 asrserver.Timing 写），交出去的说完判断里是 0（不带这两项）。
func TestVADDefaultsComeFromASR(t *testing.T) {
	settings := terminalSettingsFrom(t, nil)
	turn := settings.TurnDetectionSettings()
	if settings.VADThreshold != 0 || settings.VADSilence != 0 || turn.Threshold != 0 || turn.SilenceDurationMs != 0 {
		t.Errorf("不设时门槛、静音时长应为 0（由 haro-asr 用自己的默认值）：%+v / %+v", settings, turn)
	}
	settings = terminalSettingsFrom(t, map[string]string{"HARO_VAD_THRESHOLD": "0.6", "HARO_VAD_SILENCE": "500ms"})
	if turn := settings.TurnDetectionSettings(); turn.Threshold != 0.6 || turn.SilenceDurationMs != 500 {
		t.Errorf("设了就交出去：%+v", turn)
	}
}
