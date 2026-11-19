package config

import (
	"strings"
	"testing"
)

// asrEnv 是一组齐全的 haro-asr 配置；各用例在它上面改一项。
var asrEnv = map[string]string{
	"HARO_ASR_LISTEN": "127.0.0.1:59126", "HARO_ASR_MODEL_DIR": "/models/sensevoice", "HARO_VAD_MODEL": "/models/silero_vad.onnx",
	"HARO_KWS_MODEL_DIR": "/models/kws", "HARO_WAKE_WORDS_FILE": "/assets/wake-words.txt",
}

func setASREnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	for _, name := range []string{"HARO_ASR_LISTEN", "HARO_ASR_MODEL_DIR", "HARO_ASR_THREADS", "HARO_VAD_MODEL", "HARO_TURN_MODEL",
		"HARO_KWS_MODEL_DIR", "HARO_WAKE_WORDS_FILE", "HARO_SPEAKER_MODEL", "HARO_VOICEPRINTS_DIR", "HARO_KWS_THRESHOLD"} {
		t.Setenv(name, "") // 不受跑测试那台机器的环境影响
	}
	for name, value := range asrEnv {
		t.Setenv(name, value)
	}
	for name, value := range overrides {
		t.Setenv(name, value)
	}
}

func TestLoadASRSettings(t *testing.T) {
	setASREnv(t, nil)
	settings, err := LoadASRSettings()
	if err != nil || settings.Listen != "127.0.0.1:59126" || settings.Threads != 4 || settings.TurnModel != "" {
		t.Fatalf("齐全的配置应读得出来：%+v %v", settings, err)
	}
	for _, c := range []struct {
		name      string
		overrides map[string]string
		mention   string
	}{
		{"没设监听地址", map[string]string{"HARO_ASR_LISTEN": ""}, "HARO_ASR_LISTEN"},
		{"没设识别模型", map[string]string{"HARO_ASR_MODEL_DIR": ""}, "HARO_ASR_MODEL_DIR"},
		{"没设唤醒词模型", map[string]string{"HARO_KWS_MODEL_DIR": ""}, "HARO_KWS_MODEL_DIR"},
		{"线程数为 0", map[string]string{"HARO_ASR_THREADS": "0"}, "HARO_ASR_THREADS"},
		{"设了声纹模型没设目录", map[string]string{"HARO_SPEAKER_MODEL": "/models/campplus.onnx"}, "HARO_VOICEPRINTS_DIR"},
		{"门槛超出 0–1", map[string]string{"HARO_KWS_THRESHOLD": "1.5"}, "HARO_KWS_THRESHOLD"},
	} {
		setASREnv(t, c.overrides)
		if _, err := LoadASRSettings(); err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%s：应报错并说出 %s，得到 %v", c.name, c.mention, err)
		}
	}
}
