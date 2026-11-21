package config

import (
	"strings"
	"testing"
	"time"
)

// HARO_SERVER_URL 启动时就查：要是 http(s)://主机[:端口]；写成 ws://、不带协议、没有主机名时起不来，报错里提到这个配置项。
func TestServerURLIsValidated(t *testing.T) {
	settings := TerminalSettings{TurnDetection: "server_vad", CameraBackend: CameraBackendAuto, WakeIdle: time.Minute}
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

// 说完判断的取值按 realtimeproto 的定义查（和 haro-asr 同一份）：写错的积极度、超范围的阈值起不来；对话窗口要大于 0。
func TestTerminalTurnSettingsAreValidated(t *testing.T) {
	base := TerminalSettings{TurnDetection: "semantic_vad", TurnEagerness: "auto", VADThreshold: 0.5, CameraBackend: CameraBackendAuto,
		WakeIdle: time.Minute, ServerURL: "http://127.0.0.1:59130"}
	if err := base.validateTerminal(); err != nil {
		t.Fatalf("基线应通过：%v", err)
	}
	for name, change := range map[string]func(*TerminalSettings){
		"HARO_TURN_EAGERNESS": func(s *TerminalSettings) { s.TurnEagerness = "fast" },
		"HARO_VAD_THRESHOLD":  func(s *TerminalSettings) { s.VADThreshold = 2 },
		"HARO_WAKE_IDLE":      func(s *TerminalSettings) { s.WakeIdle = 0 },
	} {
		settings := base
		change(&settings)
		if err := settings.validateTerminal(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s 写错应起不来并写明：%v", name, err)
		}
	}
}
