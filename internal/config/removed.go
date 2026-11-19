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
}

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
