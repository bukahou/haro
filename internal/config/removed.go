package config

import (
	"fmt"
	"strconv"
)

// removedSetting 是已经删掉的配置项。stillMeans 判断写着的值是不是和现在的行为一致（写了只是多余）；nil = 任何值都只是多余。
type removedSetting struct {
	name       string
	stillMeans func(value string) bool
	removed    string // 删掉了什么
}

// removedSettings 是删掉的配置项：环境里还写着时，值要求的是已删除的行为就启动失败；只是多余就提醒可以删掉。
var removedSettings = []removedSetting{
	{name: "HARO_TURN_MODE", stillMeans: func(value string) bool { return value == "hands-free" },
		removed: "按键说话和回车切换两种对话方式已删除，只用唤醒词"},
	{name: "HARO_WAKE_WORD", stillMeans: func(value string) bool {
		on, err := strconv.ParseBool(value)
		return err == nil && on
	}, removed: "唤醒词是唯一的对话方式，不能关闭"},
	{name: "HARO_MIN_SPEECH", removed: "按键说话已删除，这个时长不再使用"},
	{name: "HARO_VOICE_API_URL", removed: "整段转写的 voice-api 识别已删除，只用流式识别"},
	{name: "HARO_ASR_MODEL_NAME", removed: "整段转写已删除，流式识别不用这个模型名"},
	{name: "HARO_CAMERA_MODE", stillMeans: func(value string) bool { return value == "on-demand" },
		removed: "摄像头常驻模式已删除，只在要看的时候才打开"},
	{name: "HARO_CAMERA_FPS", removed: "常驻模式已删除，这个帧率不再使用"},
	{name: "HARO_FRAME_MAX_AGE", removed: "常驻模式已删除，不再有「最新一帧」的时效"},
	{name: "HARO_ASR", removed: "服务端不再拉起 haro-asr：haro-asr 单独运行，服务端只按 HARO_ASR_URL 连"},
	{name: "HARO_ASR_BIN", removed: "服务端不再拉起 haro-asr，不用知道它在哪"},
	{name: "HARO_ASR_PORT", removed: "haro-asr 的监听地址改由 HARO_ASR_LISTEN 设，服务端按 HARO_ASR_URL 连"},
	{name: "HARO_PIPER_PYTHON", removed: "服务端不再拉起 Piper：Piper 单独运行，服务端只按 HARO_TTS_URL 连"},
	{name: "HARO_PIPER_MODEL", removed: "服务端不再拉起 Piper，模型由运行 Piper 的命令指定"},
	{name: "HARO_PIPER_PORT", removed: "服务端不再拉起 Piper，按 HARO_TTS_URL 连"},
	{name: "HARO_NETDIAG_DNS_NAME", removed: "网络诊断工具已删除"},
	{name: "HARO_NETDIAG_INTERNET_TARGET", removed: "网络诊断工具已删除"},
	{name: "HARO_CONTEXT_SIZE", removed: "本地模型改走 Ollama 的 OpenAI 兼容端点，上下文长度在 Ollama 那边配（OLLAMA_CONTEXT_LENGTH）"},
	{name: "HARO_MODEL_KEEP_ALIVE", removed: "本地模型改走 Ollama 的 OpenAI 兼容端点，模型常驻多久在 Ollama 那边配（OLLAMA_KEEP_ALIVE）"},
	// 本机终端的环境里可能还写着 true：只提醒，不让终端起不来（终端和服务端现在共用一份配置）
	{name: "HARO_LLM_FALLBACK", removed: "云端连不上时不再自动换本地模型，只念「连不上」和现在的时间"},
	{name: "HARO_LLM_FALLBACK_COOLDOWN", removed: "不再自动换本地模型，这个冷却时间不再使用"},
	{name: "HARO_RESEARCH_LLM", stillMeans: func(value string) bool { return value == "cloud" },
		removed: "后台调查只用云端模型（本地小模型调工具不可靠）"},
	{name: "HARO_CLOUD_LLM_PRICE_INPUT", removed: modelPricesFromCatalog},
	{name: "HARO_CLOUD_LLM_PRICE_OUTPUT", removed: modelPricesFromCatalog},
	{name: "HARO_CLOUD_LLM_PRICE_CACHE_READ", removed: modelPricesFromCatalog},
	{name: "HARO_CLOUD_LLM_PRICE_CACHE_WRITE", removed: modelPricesFromCatalog},
	{name: "HARO_STRONG_LLM_PRICE_INPUT", removed: modelPricesFromCatalog},
	{name: "HARO_STRONG_LLM_PRICE_OUTPUT", removed: modelPricesFromCatalog},
	{name: "HARO_STRONG_LLM_PRICE_CACHE_READ", removed: modelPricesFromCatalog},
	{name: "HARO_STRONG_LLM_PRICE_CACHE_WRITE", removed: modelPricesFromCatalog},
}

// modelPricesFromCatalog：云端模型的单价不再手写，启动时从模型价格目录取。
const modelPricesFromCatalog = "云端模型的单价改从模型价格目录（HARO_MODEL_PRICES_URL）取，不再手写"

// checkRemovedSettings 检查环境里还写着的已删除配置项：返回给人看的提醒；值要求已删除的行为时返回错误。
func checkRemovedSettings(lookup func(string) (string, bool)) ([]string, error) {
	var notes []string
	for _, setting := range removedSettings {
		value, ok := lookup(setting.name)
		if !ok {
			continue
		}
		if setting.stillMeans != nil && !setting.stillMeans(value) {
			return nil, fmt.Errorf("%s=%q 已不再支持：%s。请从配置里删掉它", setting.name, value, setting.removed)
		}
		notes = append(notes, fmt.Sprintf("配置项 %s 已删除（%s），现在不起作用，可以从配置里删掉", setting.name, setting.removed))
	}
	return notes, nil
}
