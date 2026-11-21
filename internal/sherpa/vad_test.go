package sherpa

import (
	"testing"
	"time"

	"github.com/bukahou/haro/internal/asrserver"
)

// VAD 的参数照传入的来（asrserver.Timing 和会话的 turn_detection），不在这里另写一份；内部缓冲装得下最长的一段。
func TestSileroConfigComesFromSession(t *testing.T) {
	session := asrserver.VoiceDetectorConfig{Threshold: 0.4, MinSilence: 300 * time.Millisecond,
		MinSpeech: 400 * time.Millisecond, MaxSpeech: 12 * time.Second}
	silero := sileroConfig("silero_vad.onnx", session).SileroVad
	if silero.Model != "silero_vad.onnx" || silero.Threshold != 0.4 || silero.MinSilenceDuration != 0.3 ||
		silero.MinSpeechDuration != 0.4 || silero.MaxSpeechDuration != 12 || silero.WindowSize != sileroWindowSize {
		t.Errorf("参数应照传入的来：%+v", silero)
	}
	if buffer := vadBuffer(session); buffer < session.MaxSpeech {
		t.Errorf("内部缓冲 %v 装不下最长的一段 %v", buffer, session.MaxSpeech)
	}
}
