package realtimeproto

import "testing"

// 转写会话的地址按结构拼：换 scheme、接路径（保留根地址里的路径）、带 intent 参数。
func TestTranscriptionURL(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:59126":       "ws://127.0.0.1:59126/v1/realtime?intent=transcription",
		"http://127.0.0.1:59126/":      "ws://127.0.0.1:59126/v1/realtime?intent=transcription",
		"https://asr.example.com/base": "wss://asr.example.com/base/v1/realtime?intent=transcription",
	} {
		if got, err := TranscriptionURL(base); err != nil || got != want {
			t.Errorf("%s → %q %v，应为 %q", base, got, err, want)
		}
	}
	if _, err := TranscriptionURL("ftp://127.0.0.1"); err == nil {
		t.Error("不是 http(s) 的地址应报错")
	}
}
