package brain

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 打字会话（Components.TextOnly，App 的打字入口）：不开识别流、不合成语音，说的话由 SubmitText 交进来，回答以文字发出。

// textItems 给打字会话交进来的话编号（日志里的 item）。
var textItems atomic.Int64

// TextInput 是打字会话收到的一句话。
type TextInput struct {
	Text       string
	Person     string // 登录的账号本人（App 里登录过，等同声纹确认）
	Language   string // App 的语言（固定的话按它选）
	Image      []byte // 这句带的图片（JPEG）；nil = 没有。只交给这一轮的模型
	Attachment string // 图片的附件编号（历史里记它）
	// Decision：这一句是主人在确认卡片上点的（Text 留空：记成按语言的「确认」「取消」）。这一轮先确认或作废，再请模型说结果
	Decision *Decision
}

// SubmitText 是打字会话收到的一句话。回答经 Events() 发出（文字，不合成语音）。同一时间只答一句：
// 上一句还在答时返回 CodeResponseInProgress。
func (s *Session) SubmitText(input TextInput) error {
	if !s.parts.TextOnly {
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedFeature, "语音会话不收打字的话")
	}
	done := make(chan struct{})
	close(done) // 打的字不用等识别
	text := input.Text
	if input.Decision != nil {
		key := PhraseCardCancel
		if input.Decision.Confirm {
			key = PhraseCardConfirm
		}
		text = s.opts.Phrases.Say(input.Language, key, nil)
	}
	job := &transcriptionJob{item: fmt.Sprintf("text-%d", textItems.Add(1)), done: done, text: text, started: time.Now(),
		speaker: input.Person, speakerChecked: true, language: input.Language, image: input.Image, attachment: input.Attachment,
		decision: input.Decision}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelResponse != nil {
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeResponseInProgress, "已有进行中的回答")
	}
	s.transcription = job
	return s.startResponseLocked()
}

// wholeText 是打字会话的「切句器」：模型写一段就原样交出一段（写给人看，不用按句子合成语音，也不去掉 Markdown）。
type wholeText struct{}

func (*wholeText) Push(text string) []string {
	if text == "" {
		return nil
	}
	return []string{text}
}

func (*wholeText) Flush() []string { return nil }
