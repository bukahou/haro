package asrserver

import "time"

// Timing 是转写会话里决定「说完没有」「是不是只叫了唤醒词」「是不是回声」的时长与门槛，集中在这里。
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
	// WakeOnlyWait：唤醒词说完就停了，等这么久没开口就按「只叫了唤醒词」处理（回「我在」）。
	// 真机上试过 2 s，显得迟钝；1 s 足够开口
	WakeOnlyWait time.Duration
	// WakeKeywordWindow：唤醒词识别报告时刻之前的这一段一定包含整个唤醒词，也是算唤醒词声纹用的那一段。
	// 实测「哈罗哈罗」约 1 s，报告延迟约 0.3 s；换更长的唤醒词要跟着加长
	WakeKeywordWindow time.Duration
	// MinWakeWindow：算唤醒词声纹至少要这么长的声音（刚播完时避开回声，剩下不够这么长就不避）。定值：
	// 实测唤醒词那 1.5 s 的声纹相似度只有整句的一半左右，再短更不稳
	MinWakeWindow time.Duration
	// MinAfterEcho：语音段剪掉开头的回声后剩下不到这么长，就当整段都是回声。定值，没有单独实测
	MinAfterEcho time.Duration
	// History：只保留最近这么长的音频（截取一句话、算声纹从这里取）。定值：比 VAD 的最长语音段（20 s）长
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
	History:           30 * time.Second,
}

func (t Timing) orDefault() Timing {
	if t == (Timing{}) {
		return DefaultTiming
	}
	return t
}

// maxWait 是 semantic_vad 判定「没说完」之后最多再等多久（按 eagerness；medium / auto / 空都按 auto）。
func (t Timing) maxWait(eagerness string) time.Duration {
	switch eagerness {
	case "low":
		return t.MaxWaitLow
	case "high":
		return t.MaxWaitHigh
	default:
		return t.MaxWaitAuto
	}
}

// samplesIn 是 d 这么长的音频有多少个采样点。
func samplesIn(d time.Duration) int { return int(d.Milliseconds()) * samplesPerMs }
