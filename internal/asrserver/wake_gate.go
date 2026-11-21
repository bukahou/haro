package asrserver

// wakeGate 是唤醒门（haro 扩展）：启用时，休眠中只跑唤醒词识别，不识别、不上报任何内容。
// 终端播放回答期间只听唤醒词（打断）；播完恢复播放前醒没醒。
type wakeGate struct {
	enabled bool
	awake   bool
	spotter KeywordSpotter // 启用时才有
	// awakeBeforePlayback：开始播放前醒没醒（播完恢复；被打断时已经醒了，不用恢复）
	awakeBeforePlayback bool
	lastWakeAt          int // 最近一次听到唤醒词的位置（采样点；排查唤醒后多出一句「Yeah.」用）
}

// listening 判断现在要不要听主人说话：没启用唤醒门、或醒着，并且终端没在播放。
func (g *wakeGate) listening(playing bool) bool { return (!g.enabled || g.awake) && !playing }

// spot 喂一块音频给唤醒词识别；没启用时什么都不做。返回听到的唤醒词（没有为空）。
func (g *wakeGate) spot(samples []float32) string {
	if !g.enabled {
		return ""
	}
	return g.spotter.AcceptWaveform(samples)
}

// woke 是听到唤醒词（或打断）：醒了，记下位置。
func (g *wakeGate) woke(at int) { g.awake, g.lastWakeAt = true, at }

// pauseForPlayback 是终端开始播放：began 为 true（之前没在播）时记下播放前醒没醒；重置唤醒词识别。
func (g *wakeGate) pauseForPlayback(began bool) {
	if began {
		g.awakeBeforePlayback = g.awake
	}
	if g.spotter != nil {
		g.spotter.Reset()
	}
}

// resumeAfterPlayback 是终端播完：恢复播放前醒没醒。
func (g *wakeGate) resumeAfterPlayback() { g.awake = g.awakeBeforePlayback }

// sleep 进入休眠：重置唤醒词识别（只在启用时会被调用）。
func (g *wakeGate) sleep() {
	g.awake = false
	g.spotter.Reset()
}

// sinceWakeMs 返回 sample 距最近一次唤醒词的毫秒数（没听到过唤醒词时为 -1）。排查用日志。
func (g *wakeGate) sinceWakeMs(sample int) int {
	if g.lastWakeAt == 0 {
		return -1
	}
	return (sample - g.lastWakeAt) / samplesPerMs
}

// close 释放唤醒词识别。
func (g *wakeGate) close() {
	if g.spotter != nil {
		g.spotter.Close()
	}
}
