package asrserver

import "encoding/binary"

// audioTimeline 是会话的音频时间轴：累计收到的采样点（「现在」），以及最近 keep 个采样点的 pcm16
// （识别一句话、算声纹、判断说完都从这里取）。
type audioTimeline struct {
	keep  int    // 最多保留多少个采样点（Timing.History）
	total int    // 累计收到的采样点
	pcm   []byte // 最近的 pcm16
	start int    // pcm[0] 对应的采样点
}

// now 是时间轴上的现在（累计收到的采样点）。
func (t *audioTimeline) now() int { return t.total }

// append 记下一块音频，返回它的浮点采样（16kHz 单声道，[-1, 1)）。超过保留范围的旧音频丢掉。
func (t *audioTimeline) append(pcm []byte) []float32 {
	t.pcm = append(t.pcm, pcm...)
	if limit := t.keep * 2; len(t.pcm) > limit {
		drop := len(t.pcm) - limit
		drop -= drop % 2
		t.pcm = append([]byte(nil), t.pcm[drop:]...)
		t.start += drop / 2
	}
	samples := pcm16ToFloat(pcm)
	t.total += len(samples)
	return samples
}

// slice 取出 [fromSample, toSample) 的 pcm16，超出保留范围的部分自动截掉。
func (t *audioTimeline) slice(fromSample, toSample int) []byte {
	from := max(fromSample, t.start) - t.start
	to := min(toSample-t.start, len(t.pcm)/2)
	if to <= from {
		return nil
	}
	return append([]byte(nil), t.pcm[from*2:to*2]...)
}

func pcm16ToFloat(pcm []byte) []float32 {
	samples := make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return samples
}
