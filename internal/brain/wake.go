package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 叫醒之后的那一句：识别流把整句原样交过来（唤醒词、打断时混进的回声都在里面），代码不删字，
// 只在这一轮的情况说明里告诉模型这些不是主人要说的，由模型自己分。只叫了唤醒词时，由模型简短应一声。

// unverifiedShortUtterance 是「叫醒的短句」最长多长：声纹只过了唤醒的门槛、没确认时，这么短的话受限回答，
// 更长的不回答。唤醒词「哈罗哈罗」说出来约 1.2–1.5 秒，留出叫完之后的一点停顿。
// 这是按现在的唤醒词真机定的时长，不从唤醒词的字推算（推算要按语言数音节）：换 haro-asr 关键词文件里的唤醒词时要重新实测。
const unverifiedShortUtterance = 2500 * time.Millisecond

const (
	// 只写「只是叫了你就应一声」时，模型把带着正事的整句也当成只是叫它：先应一声「哈罗哈罗，我在呢」再回答，
	// 有时还把这段说明念出来。所以两种情况分开写，并说清后面有话时不要先应。
	wakeNote = "主人这句话是叫醒你之后说的第一句：开头可能带着唤醒词「%s」（识别时可能写成近音字），唤醒词不是主人要说的内容。" +
		"整句只有唤醒词时，简短应一声；后面还有话时，直接回应那些话：不要先应一声（例如重复唤醒词、说「我在」），也不要提到这段说明。"
	bargeInNote = "主人这句话是在你说话时打断你说的：开头可能混着你自己刚才那句话的回声和唤醒词「%s」，这些都不是他要说的内容。" +
		"他只是叫停了你、没说别的时，简短应一声；后面还有话时，直接回应那些话，不要先应一声，也不要提到这段说明。"
	// 一开口就打断（终端有回声消除）：没有唤醒词；回声消除没消干净时会被自己的声音打断，这时接着说完
	voiceBargeInNote = "主人这句话是在你说话时开口打断你的：开头可能混着你自己刚才那句话的回声，那不是主人说的。" +
		"整句看起来都只是你自己刚才的话（被自己的声音误打断了）时，接着把被打断的话说完；主人只是让你停下、没说别的时，" +
		"简短应一声；后面还有话时，直接回应那些话，不要先应一声，也不要提到这段说明。"
	restrictedNote = "这句话的声纹没认出是谁（只过了叫醒的门槛），而且很短：只简短应一声，" +
		"不要提任何个人信息、以前聊过的事，也不要答应做任何事。"
)

// wakeNotes 是这一轮情况说明里关于唤醒词的附注（不是叫醒后的第一句时没有）。
func (s *Session) wakeNotes(job *transcriptionJob) []string {
	s.mu.Lock()
	keyword := s.lastWakeKeyword
	s.mu.Unlock()
	if keyword == "" {
		keyword = "唤醒词"
	}
	switch {
	case job.bargeIn && job.wake:
		return []string{fmt.Sprintf(bargeInNote, keyword)}
	case job.bargeIn:
		return []string{voiceBargeInNote}
	case job.wake:
		return []string{fmt.Sprintf(wakeNote, keyword)}
	}
	return nil
}

// speakRestricted 回答一句叫醒的短句，但声纹没确认（只过了唤醒的门槛）：不带工具、不带个人资料和历史，
// 这一轮也不记进任何人的历史。
func (s *Session) speakRestricted(ctx context.Context, job *transcriptionJob) realtimeproto.ResponseDone {
	s.logger.Info("声纹没确认的叫醒短句：受限回答", "text", job.text, "audio", job.audio, "like", job.tentative)
	s.rememberLanguage(job.language)
	record := newTurnRecord(job.text)
	record.restricted = true
	notes := append([]string{restrictedNote}, s.wakeNotes(job)...)
	history := []ChatMessage{{Role: RoleSystem, Text: composeSystemPrompt(s.parts.Front.Instructions, "", "", s.currentPersona())},
		{Role: RoleUser, Text: situationHeading + joinLines(notes), Synthetic: true}}
	answer, usage, err := s.speakAnswer(ctx, history, ChatMessage{Role: RoleUser, Text: job.text}, cameraView{}, record)
	return s.finishAnswer(ctx, record, answer, usage, err)
}

// rememberLanguage 记下主人最近一句话识别出的语种（不经模型、直接念的固定的话按它选语言）。
func (s *Session) rememberLanguage(language string) {
	if language == "" {
		return
	}
	s.mu.Lock()
	s.language = language
	s.mu.Unlock()
}
