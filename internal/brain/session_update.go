package brain

import "github.com/bukahou/haro/internal/realtimeproto"

// checkSessionUpdateLocked 检查 session.update 能不能照办；不能就整条拒绝（什么都不改）：
//   - 声明的输入格式要是识别流要的格式（streamFormat）：收下了，之后每一块音频都要报一次错；
//   - 识别流打开以后，不能再改说完判断（turn_detection 里 create_response 除外）和唤醒门：识别流按打开时的设置运行，
//     改了也不生效，不能悄悄忽略。要换设置就重新连接。
//
// 调用方持有 s.mu。
func (s *Session) checkSessionUpdateLocked(update realtimeproto.SessionUpdate) error {
	if format := update.InputFormat; format.SampleRate > 0 && format != streamFormat {
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedAudioFormat, "只收 %d Hz、%d 声道的输入音频，声明的是 %d Hz、%d 声道",
			streamFormat.SampleRate, streamFormat.Channels, format.SampleRate, format.Channels)
	}
	if s.streamConfig != nil && streamSettingsChanged(*s.streamConfig, update) {
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedFeature,
			"识别流已经打开：会话中途不能改说完判断（turn_detection，create_response 除外）和唤醒门（haro.wake_word），要换请重新连接")
	}
	return nil
}

// streamSettingsChanged 判断这次 session.update 要不要改已经打开的识别流的设置。没带 turn_detection = 不改说完判断；
// 唤醒门和 turn_detection 一起才有意义（打开识别流时才看），单独要求打开、而识别流没开唤醒门，也算要改。
func streamSettingsChanged(opened StreamConfig, update realtimeproto.SessionUpdate) bool {
	if update.TurnDetection == nil {
		return update.WakeWord && !opened.WakeWord
	}
	requested := *update.TurnDetection
	requested.CreateResponse = opened.TurnDetection.CreateResponse // create_response 随时可以改，不比
	return requested != opened.TurnDetection || update.WakeWord != opened.WakeWord
}
