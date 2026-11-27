package brain

import "time"

// 给外部测试（brain_test）用的内部常量：测试和代码引用同一个出处，不在测试里另抄一遍文字。
var (
	EscalatedNote             = escalatedNote
	AlreadyEscalatedNote      = alreadyEscalatedNote
	FrameRecalledNote         = frameRecalledNote
	DirectViewNote            = directViewNote
	ToolNotRunInterrupted     = toolNotRunInterrupted
	FailedBeforeSpeaking      = failedBeforeSpeaking
	InterruptedBeforeSpeaking = interruptedBeforeSpeaking
	ExpertReport              = expertReport
	ExpertDetailsHeading      = expertDetailsHeading
	EscalatedRecord           = escalatedRecord
	CaptureFailedNote         = captureFailedNote
	WakeNote                  = wakeNote
	RestrictedNote            = restrictedNote
	ToolFailedPrefix          = toolFailedPrefix
	InvalidArgumentsHint      = invalidArgumentsHint
	TransientFailureHint      = transientFailureHint
	AnswerTruncated           = answerTruncated
	ToolOverview              = toolOverview
)

// ConversationUnreadableNote 是之前的对话没读出来时给模型的附注。
var ConversationUnreadableNote = conversationUnreadableNote

// SetTranscriptionWaitTimeoutForTest 临时改「等识别结果」的期限，返回改回去的函数。
func SetTranscriptionWaitTimeoutForTest(timeout time.Duration) (restore func()) {
	previous := transcriptionWaitTimeout
	transcriptionWaitTimeout = timeout
	return func() { transcriptionWaitTimeout = previous }
}

// ExpertHistoryRecord 是看画面那次在历史里记成的样子。
func ExpertHistoryRecord(report FrameReport, spokenDirectly bool) string {
	return cameraLook{report: &report}.historyResult(spokenDirectly)
}
