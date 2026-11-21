package asrserver

// turnEnd 是说完判断：server_vad 静音即说完；semantic_vad 把这句话交给模型判断，没说完就等主人接着说，
// 等太久（waitLimit）按说完处理。
type turnEnd struct {
	semantic    bool
	detector    TurnDetector // semantic 时才有
	cutoff      float64      // 概率到这么高算说完（Timing.EndOfTurnCutoff）
	maxWait     int          // semantic 时等接着说的上限（采样点，按 eagerness）
	defaultWait int          // 其他情况（刚唤醒）等接着说的上限（采样点）
}

// waitLimit 是等主人接着说的上限（采样点）：semantic_vad 按 eagerness；其他情况用 defaultWait。
func (e turnEnd) waitLimit() int {
	if e.maxWait > 0 {
		return e.maxWait
	}
	return e.defaultWait
}

// complete 判断这一段（pcm16，含刚才的短暂静音）在语气上说完没有；返回模型给的概率（日志用）。
func (e turnEnd) complete(pcm []byte) (bool, float64, error) {
	probability, err := e.detector.EndOfTurnProbability(pcm16ToFloat(pcm))
	if err != nil {
		return false, 0, err
	}
	return probability >= e.cutoff, probability, nil
}
