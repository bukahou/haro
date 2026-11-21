package asrserver

import (
	"math"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// minAfterEchoMs：剪掉回声后剩下不到这么长，就当整段都是回声。
const minAfterEchoMs = 250

// minWakeWindowMs：算唤醒词声纹至少要这么长的声音（避开回声后太短就不避了）。
const minWakeWindowMs = 1000

// echoWindow 是回声窗口（haro 扩展）：终端播放回答期间，麦克风里主要是 haro 自己的声音。
//
// until：这个采样点之前的声音是 haro 自己的回声。播完时 = 播放结束的位置；打断时 = 打断句的开头。
// 播放期间开始、播完后才结束的语音段，只取这之后的部分——否则回声会被当成用户说的话，
// 用户紧接着说的话也会和回声并成一句（声纹被拉低、被当成陌生人丢掉）。
type echoWindow struct {
	playing bool
	until   int
	level   levelMeter // 播放期间的麦克风音量（排查打断用：设备会不会在放声音时把麦克风压低）
}

// start 是终端开始播放；返回这次是不是刚开始（之前没在播）。
func (w *echoWindow) start() bool {
	began := !w.playing
	if began {
		w.level = levelMeter{}
	}
	w.playing = true
	return began
}

// stop 是终端播完了：回声到 at 为止；返回之前是不是在播。
func (w *echoWindow) stop(at int) bool {
	if !w.playing {
		return false
	}
	w.playing, w.until = false, at
	return true
}

// interrupt 是播放中被唤醒词打断：不再算在播放（回声到哪里由打断句的开头定，见 cutAt）。
func (w *echoWindow) interrupt() { w.playing = false }

// cutAt 把回声的终点定在 at（打断句的开头：之前混着 haro 自己的声音）。
func (w *echoWindow) cutAt(at int) { w.until = at }

// trim 剪掉语音段开头的回声。剩下不到 minAfterEchoMs 时整段都当回声（ignore）；cut 表示剪过。
func (w echoWindow) trim(segment SpeechSegment) (trimmed SpeechSegment, cut, ignore bool) {
	if segment.StartSample >= w.until {
		return segment, false, false
	}
	end := segment.StartSample + segment.SampleCount
	if end-w.until < minAfterEchoMs*samplesPerMs {
		return segment, false, true
	}
	return SpeechSegment{StartSample: w.until, SampleCount: end - w.until}, true, false
}

// paddedStart 是识别一句话时音频的起点：往前多取 prefixMs，但不取进 haro 自己的回声。
func (w echoWindow) paddedStart(startSample, prefixMs int) int {
	return max(startSample-prefixMs*samplesPerMs, min(startSample, w.until))
}

// wakeWindowStart 是算唤醒词声纹的起点：唤醒词前 wakeKeywordWindowMs，刚播完时避开回声（剩下至少 minWakeWindowMs）。
// 播放中被叫（打断）时回声避不开，只能整段算。
func (w echoWindow) wakeWindowStart(now int) int {
	start := now - wakeKeywordWindowMs*samplesPerMs
	if !w.playing && w.until > start {
		start = min(w.until, now-minWakeWindowMs*samplesPerMs)
	}
	return start
}

// levelMeter 按每秒统计音量（排查用日志）：返回这一秒的均方根与峰值，满一秒时 ok = true。
type levelMeter struct {
	samples int
	sumSq   float64
	peak    float64
}

func (m *levelMeter) add(samples []float32) (rms, peak float64, ok bool) {
	for _, sample := range samples {
		value := math.Abs(float64(sample))
		m.sumSq += value * value
		m.peak = max(m.peak, value)
	}
	m.samples += len(samples)
	if m.samples < realtimeproto.TranscriptionSampleRate {
		return 0, 0, false
	}
	rms, peak = math.Sqrt(m.sumSq/float64(m.samples)), m.peak
	*m = levelMeter{}
	return rms, peak, true
}
