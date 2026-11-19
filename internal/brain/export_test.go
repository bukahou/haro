package brain

import "time"

// 给外部测试（brain_test）用的内部常量：测试和代码引用同一个出处，不在测试里另抄一遍文字。
var (
	EscalatedNote             = escalatedNote
	AlreadyEscalatedNote      = alreadyEscalatedNote
	FrameRecalledNote         = frameRecalledNote
	ToolNotRunInterrupted     = toolNotRunInterrupted
	FailedBeforeSpeaking      = failedBeforeSpeaking
	InterruptedBeforeSpeaking = interruptedBeforeSpeaking
	ExpertReport              = expertReport
	UserSaysHeading           = userSaysHeading
	CaptureFailedNote         = captureFailedNote
)

// SetTranscriptionWaitTimeoutForTest 临时改「等识别结果」的期限，返回改回去的函数。
func SetTranscriptionWaitTimeoutForTest(timeout time.Duration) (restore func()) {
	previous := transcriptionWaitTimeout
	transcriptionWaitTimeout = timeout
	return func() { transcriptionWaitTimeout = previous }
}
