package asrserver

import (
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// Timing 是转写会话里决定「说完没有」「是不是只叫了唤醒词」「是不是回声」的时长与门槛，集中在这里（VAD 的参数也在这里，
// 经 VoiceDetectorConfig 交给注入的 VAD）。
// 是在真机（USB 麦克风 + 蓝牙音箱、唤醒词「哈罗哈罗」）上调出来的，每个字段写明是怎么来的：
// 实测、参考实现，还是没有单独实测的定值。换唤醒词、换设备时按这里改；haro-asr 经 Dependencies.Timing 注入，
// 零值 = DefaultTiming。
type Timing struct {
	// SemanticSilence：semantic_vad 时 VAD 静音这么久就交给说完判断模型。实测：句子说完后的尾音（换气、拖音）
	// 约在 238 ms 后才到，等得比它短会把尾音切成新的一句（识别成「Yeah.」）；300 ms 让尾音并进同一句（每句慢约 0.1 s）
	SemanticSilence time.Duration
	// EndOfTurnCutoff：说完判断模型给的概率到这么高算说完。取自 Smart Turn 的参考实现
	EndOfTurnCutoff float64
	// MaxWaitLow / Auto / High：判定「没说完」之后最多再等多久，按 eagerness。参照 OpenAI Realtime 的
	// low 8 s / medium 4 s / high 2 s 整体缩短（面对面聊天等这么久很别扭；没有逐档实测）
	MaxWaitLow, MaxWaitAuto, MaxWaitHigh time.Duration
	// DefaultWait：不是 semantic_vad 时（例如刚唤醒、还没开口）等主人接着说的上限。定值，没有单独实测
	DefaultWait time.Duration
	// WakeOnlyWait：唤醒词说完就停了，等这么久没开口就把这一句（只有唤醒词）交出去，由服务端的模型简短应一声。
	// 真机上试过 2 s，显得迟钝；1 s 足够开口
	WakeOnlyWait time.Duration
	// WakeKeywordWindow：唤醒词识别报告时刻之前的这一段一定包含整个唤醒词，也是算唤醒词声纹用的那一段。
	// 实测「哈罗哈罗」约 1 s，报告延迟约 0.3 s。按现在的唤醒词真机定的，不从唤醒词的字推算（推算要按语言数音节）：
	// 换关键词文件里的唤醒词时要重新实测（和 brain 的 unverifiedShortUtterance 一起）
	WakeKeywordWindow time.Duration
	// MinWakeWindow：算唤醒词声纹至少要这么长的声音（刚播完时避开回声，剩下不够这么长就不避）。定值：
	// 实测唤醒词那 1.5 s 的声纹相似度只有整句的一半左右，再短更不稳
	MinWakeWindow time.Duration
	// MinAfterEcho：语音段剪掉开头的回声后剩下不到这么长，就当整段都是回声。定值，没有单独实测
	MinAfterEcho time.Duration
	// BargeInSpeech：一开口就打断（turn_detection.interrupt_response）时，播放中连续说话这么久才算打断——回声消除后
	// 残留的回声、咳嗽一声不打断；越长停得越慢。初始值，没有真机实测：接上回声消除后在真机上调
	BargeInSpeech time.Duration
	// VADThreshold / VADSilence：会话的 turn_detection 没给语音概率门槛 / 静音时长时用的值（终端不设 HARO_VAD_THRESHOLD、
	// HARO_VAD_SILENCE 时就不给，用的是这里：默认值只写在这里）。门槛取自 Silero VAD 参考实现的默认值；静音时长是定值
	// （实测：静音 700 ms 时判定说完要 716–788 ms，即它加上 VAD 的判定延迟）
	VADThreshold float64
	VADSilence   time.Duration
	// VADMinSpeech：短于这么长的声音（咳嗽、碰桌子）不算说话。取自 Silero VAD 参考实现的默认值，没有单独实测
	VADMinSpeech time.Duration
	// VADMaxSpeech：一口气说到这么长，VAD 设法在下一个短停顿处切段（sherpa-onnx 的做法是临时提高门槛，所以一段可能比它
	// 略长）。定值，没有单独实测
	VADMaxSpeech time.Duration
	// MergeLimit：semantic_vad 把停顿前后的几段并成一句，并到这么长（从这句开头到刚结束的那一段）就不再等接着说、按说完
	// 处理，后面的话另成一句。不设上限时一句可以无限长，保留的音频（History）就装不下了。定值：对着音箱说的一句话
	// 很少有这么长，没有单独实测
	MergeLimit time.Duration
	// History：只保留最近这么长的音频（截取一句话、算声纹从这里取）。要装得下可能合并出的最长句（longestUtterance）：
	// MergeLimit 30 s + 最长的等待 3 s（MaxWaitLow）+ 最后并进来的一段 VADMaxSpeech 20 s = 53 s，再留 7 s 给 VAD
	// 超过最长语音段之后切段的延迟、识别时往前多取的 prefix_padding（终端默认 0.3 s），取 60 s。万一还是装不下，
	// 截掉开头时记一条警告
	History time.Duration
}

// DefaultTiming 是现在在用的值（来历见 Timing 的各字段）。
var DefaultTiming = Timing{
	SemanticSilence:   300 * time.Millisecond,
	EndOfTurnCutoff:   0.5,
	MaxWaitLow:        3 * time.Second,
	MaxWaitAuto:       2 * time.Second,
	MaxWaitHigh:       time.Second,
	DefaultWait:       2 * time.Second,
	WakeOnlyWait:      time.Second,
	WakeKeywordWindow: 1500 * time.Millisecond,
	MinWakeWindow:     time.Second,
	MinAfterEcho:      250 * time.Millisecond,
	BargeInSpeech:     300 * time.Millisecond,
	VADThreshold:      0.5,
	VADSilence:        700 * time.Millisecond,
	VADMinSpeech:      250 * time.Millisecond,
	VADMaxSpeech:      20 * time.Second,
	MergeLimit:        30 * time.Second,
	History:           60 * time.Second,
}

func (t Timing) orDefault() Timing {
	if t == (Timing{}) {
		return DefaultTiming
	}
	return t
}

// maxWait 是 semantic_vad 判定「没说完」之后最多再等多久（按 eagerness；medium / auto / 空都按 auto，
// 别的取值在收到 session.update 时已经拒绝了，见 realtimeproto.TurnDetection.Validate）。
func (t Timing) maxWait(eagerness string) time.Duration {
	switch eagerness {
	case realtimeproto.EagernessLow:
		return t.MaxWaitLow
	case realtimeproto.EagernessHigh:
		return t.MaxWaitHigh
	default:
		return t.MaxWaitAuto
	}
}

// longestUtterance 是可能合并出的最长一句（History 要装得下它）：semantic_vad 下一句没并到 MergeLimit 时还会再等
// 一次接着说（最多等最长的那个等待），再并进来一段（最长 VADMaxSpeech）。其他情况（server_vad、刚唤醒）都比这短。
func (t Timing) longestUtterance() time.Duration {
	return t.MergeLimit + max(t.MaxWaitLow, t.MaxWaitAuto, t.MaxWaitHigh, t.DefaultWait, t.WakeOnlyWait) + t.VADMaxSpeech
}

// voiceDetectorConfig 是一个会话的 VAD 参数：语音概率门槛、静音时长按会话的 turn_detection（没给时用这里的默认值；
// semantic_vad 的静音时长由调用方先换成 SemanticSilence），最短 / 最长语音段按这里。
func (t Timing) voiceDetectorConfig(turn realtimeproto.TurnDetection) VoiceDetectorConfig {
	config := VoiceDetectorConfig{Threshold: turn.Threshold, MinSilence: time.Duration(turn.SilenceDurationMs) * time.Millisecond,
		MinSpeech: t.VADMinSpeech, MaxSpeech: t.VADMaxSpeech}
	if config.Threshold <= 0 {
		config.Threshold = t.VADThreshold
	}
	if config.MinSilence <= 0 {
		config.MinSilence = t.VADSilence
	}
	return config
}

// samplesIn 是 d 这么长的音频有多少个采样点。
func samplesIn(d time.Duration) int { return int(d.Milliseconds()) * samplesPerMs }
