package brain

import "strings"

// 打断后的历史（v3 I-2，与 Realtime 的 conversation.item.truncate 一致）：用户只听到了回答的前一部分，
// 历史里就只记这一部分——否则模型会以为用户听完了整段回答。
//
// 服务端记下这次回答每一句在播放时间轴上的起止（按发给终端的音频累计）；终端被打断时报告「播了多少毫秒」，
// 开始时间早于这个点的句子算听到了（听到一半的句子也算）。
// 两种时机：
//   - 回答还在生成时被打断：取消时按截断点记历史（rememberInterrupted）；
//   - 回答已生成完、声音还在终端播放时被打断：回头改历史里这次回答的最后一条（truncateLastAnswerLocked）。

// spokenSegment 是一句已经发给终端的话在播放时间轴上的位置。
type spokenSegment struct {
	text           string
	startMs, endMs int64
}

// markSpoken 记下开始发一句新的话（在它的第一段声音发出之前调用）。
func (s *Session) markSpoken(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var start int64
	if n := len(s.spoken); n > 0 {
		start = s.spoken[n-1].endMs
	}
	s.spoken = append(s.spoken, spokenSegment{text: text, startMs: start, endMs: start})
}

// extendSpoken 把刚发出的一段声音算进当前这句的时长。
func (s *Session) extendSpoken(durationMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.spoken); n > 0 {
		s.spoken[n-1].endMs += durationMs
	}
}

// truncate 处理 conversation.item.truncate：回答还在进行就记下截断点，否则直接改历史。
func (s *Session) truncate(audioEndMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncateAtMs = audioEndMs
	if s.cancelResponse == nil { // 回答已经结束（声音还在终端播）
		s.truncateLastAnswerLocked()
	}
}

// truncateLastAnswerLocked 把这次回答记进历史的最后一条（回答文字）改成用户听到的部分。调用方持有 s.mu。
func (s *Session) truncateLastAnswerLocked() {
	if !s.answeredInResponse || s.truncateAtMs < 0 {
		return
	}
	heard := heardText(s.spoken, s.truncateAtMs)
	if s.conversation.replaceLastAnswer(heard) {
		s.logger.Info("被打断：历史改为只记听到的部分", "played_ms", s.truncateAtMs, "heard", heard)
	}
}

// rememberInterrupted 回答还在生成时被打断：按截断点记这一轮。没收到截断（例如 Ctrl+C 取消）时和原来一样不记。
func (s *Session) rememberInterrupted(question string) {
	s.mu.Lock()
	at := s.truncateAtMs
	heard := heardText(s.spoken, at)
	s.mu.Unlock()
	if at < 0 {
		return
	}
	s.remember(question, nil, heard)
	s.logger.Info("被打断：历史只记听到的部分", "played_ms", at, "heard", heard)
}

// interruptedBeforeSpeaking 是还没说出一个字就被打断时，历史里这一轮的回答。
const interruptedBeforeSpeaking = "（还没说出口就被主人打断了）"

// heardText 返回开始时间早于 atMs 的那些句子（听到一半的也算）。
func heardText(spoken []spokenSegment, atMs int64) string {
	var heard strings.Builder
	for _, segment := range spoken {
		if segment.startMs < atMs {
			heard.WriteString(segment.text)
		}
	}
	if heard.Len() == 0 {
		return interruptedBeforeSpeaking
	}
	return heard.String()
}
