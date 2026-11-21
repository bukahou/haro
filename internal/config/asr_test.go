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
	for _, name := range []string{"HARO_ASR_LISTEN", "HARO_ASR_MODEL_DIR", "HARO_ASR_MODEL_FILE", "HARO_ASR_THREADS", "HARO_VAD_MODEL",
		"HARO_TURN_MODEL", "HARO_KWS_MODEL_DIR", "HARO_KWS_ENCODER_FILE", "HARO_KWS_DECODER_FILE", "HARO_KWS_JOINER_FILE",
		"HARO_WAKE_WORDS_FILE", "HARO_SPEAKER_MODEL", "HARO_SPEAKER_THREADS", "HARO_VOICEPRINTS_DIR", "HARO_KWS_THRESHOLD"} {
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
		{"声纹线程数为 0", map[string]string{"HARO_SPEAKER_THREADS": "0"}, "HARO_SPEAKER_THREADS"},
		{"设了声纹模型没设目录", map[string]string{"HARO_SPEAKER_MODEL": "/models/campplus.onnx"}, "HARO_VOICEPRINTS_DIR"},
		{"门槛超出 0–1", map[string]string{"HARO_KWS_THRESHOLD": "1.5"}, "HARO_KWS_THRESHOLD"},
	} {
		setASREnv(t, c.overrides)
		if _, err := LoadASRSettings(); err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%s：应报错并说出 %s，得到 %v", c.name, c.mention, err)
		}
	}
}

// 模型文件由配置给出：默认是现在实际在用的那几个（和镜像里放的一致，集群、开发机不用改配置）；设了就用设的。
func TestASRModelFiles(t *testing.T) {
	setASREnv(t, nil)
	settings, err := LoadASRSettings()
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][2]string{
		"HARO_ASR_MODEL_FILE":   {settings.ModelFile, "model.int8.onnx"},
		"HARO_KWS_ENCODER_FILE": {settings.KWSEncoderFile, "encoder-epoch-13-avg-2-chunk-16-left-64.int8.onnx"},
		"HARO_KWS_DECODER_FILE": {settings.KWSDecoderFile, "decoder-epoch-13-avg-2-chunk-16-left-64.onnx"},
		"HARO_KWS_JOINER_FILE":  {settings.KWSJoinerFile, "joiner-epoch-13-avg-2-chunk-16-left-64.int8.onnx"},
	} {
		if got[0] != got[1] {
			t.Errorf("%s 默认应为 %s，实际 %q", name, got[1], got[0])
		}
	}
	if settings.SpeakerThreads != 2 {
		t.Errorf("HARO_SPEAKER_THREADS 默认应为 2，实际 %d", settings.SpeakerThreads)
	}

	setASREnv(t, map[string]string{"HARO_ASR_MODEL_FILE": "model.onnx", "HARO_KWS_ENCODER_FILE": "encoder-chunk-8.onnx",
		"HARO_KWS_DECODER_FILE": "decoder-chunk-8.onnx", "HARO_KWS_JOINER_FILE": "joiner-chunk-8.onnx", "HARO_SPEAKER_THREADS": "1"})
	settings, err = LoadASRSettings()
	if err != nil || settings.ModelFile != "model.onnx" || settings.KWSEncoderFile != "encoder-chunk-8.onnx" ||
		settings.KWSDecoderFile != "decoder-chunk-8.onnx" || settings.KWSJoinerFile != "joiner-chunk-8.onnx" || settings.SpeakerThreads != 1 {
		t.Errorf("设了就用设的：%+v %v", settings, err)
	}
}
