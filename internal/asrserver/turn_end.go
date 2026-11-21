package asrserver

import "time"

const (
	// semantic_vad：VAD 静音这么久就交给模型判断。太短时，句子说完后的尾音（换气、拖音，实测约在 238 ms 后）
	// 会被切成新的一句、识别成「Yeah.」；300 ms 让尾音并进同一句（每句慢约 0.1 s）。
	semanticSilenceMs = 300
	endOfTurnCutoff   = 0.5  // 模型概率 ≥ 0.5 算说完（与 Smart Turn 参考实现一致）
	defaultWaitMs     = 2000 // 不是 semantic_vad（例如刚唤醒还没开口）时等接着说的上限
)

// semanticMaxWait 是 semantic_vad 判断「没说完」之后最多再等多久（按 eagerness）。
// 官方 Realtime 为 low 8s / medium 4s / high 2s；面对面聊天等太久很别扭，这里整体缩短。
func semanticMaxWait(eagerness string) time.Duration {
	switch eagerness {
	case "low":
		return 3 * time.Second
	case "high":
		return time.Second
	default: // "medium" / "auto" / 空
		return 2 * time.Second
	}
}

// turnEnd 是说完判断：server_vad 静音即说完；semantic_vad 把这句话交给模型判断，没说完就等主人接着说，
// 等太久（waitLimit）按说完处理。
type turnEnd struct {
	semantic bool
	detector TurnDetector // semantic 时才有
	maxWait  int          // semantic 时等接着说的上限（采样点）
}

// waitLimit 是等主人接着说的上限（采样点）：semantic_vad 按 eagerness；其他情况（刚唤醒）默认 2 秒。
func (e turnEnd) waitLimit() int {
	if e.maxWait > 0 {
		return e.maxWait
	}
	return defaultWaitMs * samplesPerMs
}

// complete 判断这一段（pcm16，含刚才的短暂静音）在语气上说完没有；返回模型给的概率（日志用）。
func (e turnEnd) complete(pcm []byte) (bool, float64, error) {
	probability, err := e.detector.EndOfTurnProbability(pcm16ToFloat(pcm))
	if err != nil {
		return false, 0, err
	}
	return probability >= endOfTurnCutoff, probability, nil
}
