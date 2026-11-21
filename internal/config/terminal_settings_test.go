package config

import (
	"strings"
	"testing"
)

// HARO_SERVER_URL 启动时就查：要是 http(s)://主机[:端口]；写成 ws://、不带协议、没有主机名时起不来，报错里提到这个配置项。
func TestServerURLIsValidated(t *testing.T) {
	settings := TerminalSettings{TurnDetection: "server_vad", CameraBackend: CameraBackendAuto}
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
