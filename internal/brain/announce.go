package brain

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 服务端自己开口（v9 D2，协议 v4）：到点的提醒不等主人说话，由服务端直接说出来。
//
//   - 只在这台终端没在播、服务端没在答、主人没在说的时候开口：终端的回答事件里没有回答编号，
//     开口的声音不能和别的回答混在一起；主人说着话时开口会打断他。之前的事一结束就接着看要不要开口。
//   - 说的话不经模型，直接合成（云端模型连不上照样说）；合成用不了时改让终端播提醒的提示音（D2 c）。
//   - 说出的话记进这个人的对话历史，前面标上它是什么（Note），主人接着说「再过十分钟提醒我」时模型知道刚才说了什么。
//     播的时候被打断，照回答的规则只记听到的部分。

// Announcement 是服务端自己开口要说的一句话。
type Announcement struct {
	Reason string // 为什么开口（realtimeproto.UnpromptedReminder 等）：终端据此知道这不是在回答
	Person string // 说给谁：记进他的对话
	Text   string // 要说的话（情绪标记会去掉）
	// Note 是记进历史时放在前面的一句说明（系统附注，不是主人说的话），例如「到点提醒 #3」
	Note string
	// Done 在这句说完、被打断、改成提示音或没能说之后调用一次（在会话的 goroutine 里，不能阻塞）。可以为 nil
	Done func(AnnouncementOutcome)
}

// AnnouncementOutcome 是一次开口的结果。
type AnnouncementOutcome int

const (
	// AnnouncementSpoken：说出来了（播的时候被主人打断也算：他已经听到了开头）。
	AnnouncementSpoken AnnouncementOutcome = iota
	// AnnouncementChimed：语音合成用不了，终端播了提醒的提示音。
	AnnouncementChimed
	// AnnouncementDropped：没能说：会话在开口之前关了，或者合成失败、提示音也没发出去。可以之后再试
	AnnouncementDropped
)

// maxQueuedAnnouncements：一台终端最多排着几句等开口（同一时刻几条提醒到点）。再多的交不出去，由调度那边按没送到处理。
// 定值：家里同时到点的提醒远到不了这个数
const maxQueuedAnnouncements = 4

// announce 把一句话排进这台终端的开口队列；条件允许就马上开口。会话关了或排满时返回 false。
func (s *Session) announce(announcement Announcement) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return false
	default:
	}
	if len(s.announcements) >= maxQueuedAnnouncements {
		s.logger.Warn("排着等开口的话太多：这句不收", "reason", announcement.Reason)
		return false
	}
	s.announcements = append(s.announcements, announcement)
	s.startAnnouncementLocked()
	return true
}

// startAnnouncementLocked 在可以开口的时候开口说排着的第一句：没在答（也没有判定说完、等着回答的那句）、
// 主人没在说、终端没在播。调用方持有 s.mu；凡是这几个条件可能刚变成满足的地方都调用它。
func (s *Session) startAnnouncementLocked() {
	if len(s.announcements) == 0 || s.cancelResponse != nil || s.transcription != nil || s.speech != nil || s.playing {
		return
	}
	select {
	case <-s.closed:
		return
	default:
	}
	announcement := s.announcements[0]
	s.announcements = s.announcements[1:]
	ctx, cancel := context.WithCancel(s.lifetime)
	s.cancelResponse = cancel
	s.announcing = true
	s.responses.Add(1)
	go func() {
		defer s.responses.Done()
		s.runAnnouncement(ctx, announcement)
	}()
}

// runAnnouncement 开口说一句：和回答一样恰好一个 response.created（带上为什么开口）和一个 response.done。
func (s *Session) runAnnouncement(ctx context.Context, announcement Announcement) {
	s.emit(realtimeproto.ResponseCreated{Unprompted: announcement.Reason})
	s.beginResponse("")
	conversation := s.parts.Conversations.Conversation(announcement.Person)
	s.mu.Lock()
	s.conversation = conversation // 说给他听：记进他的对话，被打断时也改他的
	s.mu.Unlock()
	s.logger.Info("服务端自己开口", "reason", announcement.Reason, "person", conversation.Person(), "text", announcement.Text)

	done := s.speakFixedReply(ctx, announcement.Text)
	s.mu.Lock()
	spokeAny := len(s.spoken) > 0
	heard := stripEmotionTags(announcement.Text)
	if done.Status == realtimeproto.StatusCancelled && s.truncateAtMs >= 0 { // 说到一半被打断：只记听到的部分
		heard = heardText(s.spoken, s.truncateAtMs, 0)
		if heard == "" {
			heard = interruptedBeforeSpeaking
		}
	}
	s.answerRound = 0
	s.mu.Unlock()
	if spokeAny {
		s.rememberTurn(time.Now(), []ChatMessage{
			{Role: RoleUser, Text: situationHeading + announcement.Note, Synthetic: true},
			{Role: RoleAssistant, Text: heard},
		}, 0, false) // 主动开口不请求模型：没有用量，也不算没报
	}

	s.emit(done)
	outcome := AnnouncementSpoken
	switch {
	case done.Status == realtimeproto.StatusFailed: // 合成用不了：这次开口结束以后，让终端播提醒的提示音
		outcome = AnnouncementDropped
		if s.tryEmit(realtimeproto.HaroNotice{Kind: realtimeproto.NoticeReminder}) {
			outcome = AnnouncementChimed
		}
	case !spokeAny: // 一句都没发出去（会话在关）
		outcome = AnnouncementDropped
	}
	s.mu.Lock()
	s.cancelResponse()
	s.cancelResponse, s.announcing = nil, false
	changed := s.playbackCutLocked() // 已经记进历史之后才收到截断：回头改
	s.mu.Unlock()
	if changed {
		conversation.persist()
	}
	s.logger.Info("开口结束", "reason", announcement.Reason, "status", done.Status, "outcome", outcomeLabel(outcome))
	if announcement.Done != nil {
		announcement.Done(outcome)
	}
	s.mu.Lock()
	var pendingErr error
	if s.autoRespond {
		pendingErr = s.startPendingResponseLocked() // 开口时主人说完的那句（终端播完才真的开始）
	}
	s.startAnnouncementLocked() // 还有排着的就接着说（有要回答的那句时它挡着）
	s.mu.Unlock()
	if pendingErr != nil {
		s.logger.Warn("开口结束后没能开始回答留着的那句", "err", pendingErr)
		s.emit(realtimeproto.ErrorEventFor("", pendingErr))
	}
}

// dropAnnouncements 会话关闭时：排着还没说的都算没能说（由调度那边之后再试）。
func (s *Session) dropAnnouncements() {
	s.mu.Lock()
	queued := s.announcements
	s.announcements = nil
	s.mu.Unlock()
	for _, announcement := range queued {
		if announcement.Done != nil {
			announcement.Done(AnnouncementDropped)
		}
	}
}

func outcomeLabel(outcome AnnouncementOutcome) string {
	switch outcome {
	case AnnouncementSpoken:
		return "说出来了"
	case AnnouncementChimed:
		return "合成用不了，播了提示音"
	default:
		return "没能说"
	}
}
