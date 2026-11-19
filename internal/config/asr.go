package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// ASRSettings 是语音识别服务 haro-asr 的配置。haro-asr 和服务端各自运行（集群里是同一个 Pod 的两个容器），
// 只读这些项；服务端只认它的地址（HARO_ASR_URL）。
type ASRSettings struct {
	Listen         string `env:"HARO_ASR_LISTEN,required,notEmpty"`    // 监听地址，例如 127.0.0.1:59126（只对本机开放）
	ModelDir       string `env:"HARO_ASR_MODEL_DIR,required,notEmpty"` // SenseVoice 模型目录（含 model.int8.onnx 与 tokens.txt）
	Threads        int    `env:"HARO_ASR_THREADS" envDefault:"4"`      // 推理线程数（实测 4 线程每句 24–35ms，再多收益很小）
	Language       string `env:"HARO_ASR_LANGUAGE" envDefault:"auto"`  // auto / zh / en / ja / ko / yue
	VADModel       string `env:"HARO_VAD_MODEL,required,notEmpty"`     // Silero VAD（silero_vad.onnx）：流式转写要用
	TurnModel      string `env:"HARO_TURN_MODEL"`                      // Smart Turn v3（smart-turn-v3.2-cpu.onnx）；空 = 不支持 semantic_vad
	OnnxruntimeLib string `env:"HARO_ONNXRUNTIME_LIB" envDefault:"libonnxruntime.so"`

	// 唤醒词是唯一的对话方式：模型和关键词文件都必填
	KWSModelDir   string  `env:"HARO_KWS_MODEL_DIR,required,notEmpty"`   // sherpa-onnx 唤醒词模型目录
	WakeWordsFile string  `env:"HARO_WAKE_WORDS_FILE,required,notEmpty"` // 关键词文件（每行：拼音单元 … [:加分] [#门槛] @显示名）
	KWSThreshold  float64 `env:"HARO_KWS_THRESHOLD" envDefault:"0.25"`   // 默认触发门槛（关键词文件里写了 #门槛 的以文件为准）
	KWSScore      float64 `env:"HARO_KWS_SCORE" envDefault:"1.0"`        // 默认加分（关键词文件里写了 :加分 的以文件为准）

	// 声纹：只有登记过的人能唤醒、才会被回答。不设模型 = 关闭；设了但还没人登记 = 不过滤
	SpeakerModel     string  `env:"HARO_SPEAKER_MODEL"`                      // sherpa-onnx 说话人识别模型（CAM++）
	VoiceprintsDir   string  `env:"HARO_VOICEPRINTS_DIR"`                    // 登记的声纹（终端 haro -enroll 经服务端登记生成；生物特征，只在本地）
	SpeakerThreshold float64 `env:"HARO_SPEAKER_THRESHOLD" envDefault:"0.5"` // 相似度达到这个值才算认出来
	// 唤醒词只有 1 秒多，声纹算不稳（真机：同一人同一位置，整句 0.73、唤醒词 0.25–0.38）：唤醒用低一些的门槛。
	// 只过了这个门槛的唤醒，接着说的话仍要达到 HARO_SPEAKER_THRESHOLD 才回答
	WakeSpeakerThreshold float64 `env:"HARO_WAKE_SPEAKER_THRESHOLD" envDefault:"0.2"`
}

// LoadASRSettings 读取并校验 haro-asr 的配置。
func LoadASRSettings() (ASRSettings, error) {
	settings, err := env.ParseAs[ASRSettings]()
	if err != nil {
		return settings, err
	}
	return settings, settings.validate()
}

func (s ASRSettings) validate() error {
	if s.Threads < 1 {
		return fmt.Errorf("HARO_ASR_THREADS 至少是 1，现在是 %d", s.Threads)
	}
	if s.SpeakerModel != "" && s.VoiceprintsDir == "" {
		return fmt.Errorf("设了 HARO_SPEAKER_MODEL 就要设 HARO_VOICEPRINTS_DIR（登记的声纹放在哪）")
	}
	for name, threshold := range map[string]float64{"HARO_KWS_THRESHOLD": s.KWSThreshold, "HARO_SPEAKER_THRESHOLD": s.SpeakerThreshold,
		"HARO_WAKE_SPEAKER_THRESHOLD": s.WakeSpeakerThreshold} {
		if threshold < 0 || threshold > 1 {
			return fmt.Errorf("%s 要在 0–1 之间，现在是 %v", name, threshold)
		}
	}
	return nil
}
