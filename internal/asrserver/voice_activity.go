package asrserver

// voiceActivity 是静音检测：VAD 判断现在有没有在说话、哪些语音段已经结束；记下最近一段连续语音从哪开始。
type voiceActivity struct {
	detector    VoiceDetector
	speaking    bool // 上一块音频时在不在说话
	speechStart int  // 最近一段连续语音的开头（休眠时也记录，只用来限定唤醒句的范围）
}

// feed 喂一块音频，返回现在在不在说话。speaking 由调用方处理完这一块之后再更新（settle）。
func (v *voiceActivity) feed(samples []float32) bool {
	v.detector.AcceptWaveform(samples)
	return v.detector.IsSpeech()
}

// begins 判断这一块是不是一段连续语音的开头；是的话记下开头的位置。
func (v *voiceActivity) begins(speaking bool, now int) bool {
	if speaking && !v.speaking {
		v.speechStart = now
		return true
	}
	return false
}

// settle 记下这一块之后在不在说话。
func (v *voiceActivity) settle(speaking bool) { v.speaking = speaking }

// segments 取出已经结束的语音段。
func (v *voiceActivity) segments() []SpeechSegment { return v.detector.PopSegments() }
