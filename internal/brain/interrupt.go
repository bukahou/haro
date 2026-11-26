package brain

import "strings"

// 打断后的历史（与 Realtime 的 conversation.item.truncate 一致）：用户只听到了回答的前一部分，
// 历史里就只记这一部分——否则模型会以为用户听完了整段回答。
//
// 服务端记下这次回答每一句在播放时间轴上的起止（按发给终端的音频累计），以及它属于哪一段（调用工具前说的、最后说的）；
// 终端被打断时报告「播了多少毫秒」，开始时间早于这个点的句子算听到了（听到一半的句子也算）。
// 历史里最后一条回答只放最后一段听到的（调用工具前说的话在那条工具调用的消息里）。两种时机：
//   - 回答还在生成时被打断：收尾时按截断点整理这一轮（closeTurn）；
//   - 回答已生成完、声音还在终端播放时被打断：回头改历史里这次回答的最后一条（truncateLastAnswerLocked）。

// spokenSegment 是一句已经发给终端的话在播放时间轴上的位置。
type spokenSegment struct {
	text           string
	round          int // 属于这一轮回答的第几段（和 turnRecord 的段号一致）
	startMs, endMs int64
}

// markSpoken 记下开始发一句新的话（在它的第一段声音发出之前调用）。
func (s *Session) markSpoken(text string, round int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var start int64
	if n := len(s.spoken); n > 0 {
		start = s.spoken[n-1].endMs
	}
	s.spoken = append(s.spoken, spokenSegment{text: text, round: round, startMs: start, endMs: start})
}

// extendSpoken 把刚发出的一段声音算进当前这句的时长。
func (s *Session) extendSpoken(durationMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.spoken); n > 0 {
		s.spoken[n-1].endMs += durationMs
	}
}

// truncate 处理 conversation.item.truncate：回答还在进行就记下截断点（收尾时用），否则直接改历史。
func (s *Session) truncate(audioEndMs int64) {
	s.mu.Lock()
	s.truncateAtMs = audioEndMs
	changed := false
	if s.cancelResponse == nil { // 回答已经结束（声音还在终端播）
		changed = s.playbackCutLocked()
	}
	conversation := s.conversation
	s.mu.Unlock()
	if changed { // 放开会话锁再写盘：写盘可能要等别的保存，期间音频照常转发
		conversation.persist()
	}
}

// playbackCutLocked：已经说完的这次回答在终端播放中被打断了。附上的情况没听完，不算告诉了（之后再附上）；
// 历史里的回答改成听到的部分。没有截断时什么也不做。调用方持有 s.mu；返回 true 时调用方放开锁后要写盘（persist）。
func (s *Session) playbackCutLocked() bool {
	if s.truncateAtMs < 0 {
		return false
	}
	if len(s.pendingDelivered) > 0 || len(s.pendingInterrupted) > 0 {
		cut := s.pendingInterrupted
		s.pendingDelivered, s.pendingInterrupted = nil, nil
		s.logger.Info("播放中被打断：这一轮附上的情况没听完，之后再附上", "item", s.responseItem, "played_ms", s.truncateAtMs)
		for _, interrupted := range cut { // 只记个数（BriefingCuts），不回头调用会话
			interrupted()
		}
	}
	return s.truncateLastAnswerLocked()
}

// truncateLastAnswerLocked 把这次回答记进历史的最后一条（回答文字）改成用户听到的部分；已经是这样就不动。
// 调用方持有 s.mu；返回改没改（改了要在放开锁后写盘）。
func (s *Session) truncateLastAnswerLocked() bool {
	if !s.answeredInResponse || s.truncateAtMs < 0 {
		return false
	}
	heard := heardText(s.spoken, s.truncateAtMs, s.answerRound)
	if heard == "" {
		heard = interruptedBeforeSpeaking
	}
	if !s.conversation.replaceLastAnswer(heard) {
		return false
	}
	s.logger.Info("被打断：历史改为只记听到的部分", "item", s.responseItem, "played_ms", s.truncateAtMs, "heard", heard)
	return true
}

// interruptedBeforeSpeaking 是还没说出一个字就被打断时，历史里这一轮的回答。
const interruptedBeforeSpeaking = "（还没说出口就被主人打断了）"

// heardText 返回第 round 段里开始时间早于 atMs 的那些句子（听到一半的也算）；一句也没有时为空。
func heardText(spoken []spokenSegment, atMs int64, round int) string {
	var heard strings.Builder
	for _, segment := range spoken {
		if segment.round == round && segment.startMs < atMs {
			heard.WriteString(segment.text)
		}
	}
	return heard.String()
}

// spokenText 返回第 round 段里已经发给终端的句子（出错时这些是主人听到了的）；一句也没有时为空。
func spokenText(spoken []spokenSegment, round int) string {
	var said strings.Builder
	for _, segment := range spoken {
		if segment.round == round {
			said.WriteString(segment.text)
		}
	}
	return said.String()
}
