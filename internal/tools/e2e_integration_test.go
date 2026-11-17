//go:build integration

// 真实模型 + 内置工具的端到端对话（默认不跑；语音用文字代替）：
//
//	HARO_TEST_IMAGE=/path/frame.jpg go test -tags integration -run ConversationLive -v ./internal/tools/
package tools

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"haro/internal/brain"
	"haro/internal/llm"
	"haro/internal/netdiag"
	"haro/internal/notes"
	"haro/internal/realtimeproto"
	"haro/internal/speech"
)

type textRecognizer struct {
	mu   sync.Mutex
	text string
}

func (r *textRecognizer) Recognize(context.Context, realtimeproto.PCMAudio) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.text, nil
}

type textSynthesizer struct{}

func (textSynthesizer) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}

func TestConversationLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := notes.Open(ctx, filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// 与正式运行一致：记事本、网络诊断都内置（v5）
	builtins := append(NoteTools(store, logger), NetworkDiagnostics(netdiag.NewChecker("www.cloudflare.com", "1.1.1.1:443"))...)
	box, err := NewToolbox(ctx, nil, logger, builtins...)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	recognizer := &textRecognizer{}
	session := brain.NewSession(brain.Components{
		Recognizer: recognizer, Synthesizer: textSynthesizer{}, Tools: box,
		Model:        testChatModel(t),
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	defer session.Close()
	turns := []string{"帮我记一下，车停在C3的7号位。", "我的车停在哪了？", "现在几点了？", "家里网络正常吗？",
		"我最近都记了些什么？", "把车停车位那条记录删了。", "确认", "你能看到我吗？"}
	_ = session.Send(ctx, realtimeproto.SessionUpdate{Instructions: brain.DefaultInstructions})
	if conversation := os.Getenv("HARO_TEST_CONVERSATION"); conversation != "" {
		turns = strings.Split(conversation, "|")
	}
	image, _ := os.ReadFile(os.Getenv("HARO_TEST_IMAGE"))

	for _, text := range turns {
		recognizer.mu.Lock()
		recognizer.text = text
		recognizer.mu.Unlock()
		started := time.Now()
		_ = session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{
			Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 3200)}})
		_ = session.Send(ctx, realtimeproto.InputAudioBufferCommit{})
		if len(image) > 0 {
			_ = session.Send(ctx, realtimeproto.ConversationItemCreate{Item: realtimeproto.ConversationItem{Role: "user",
				Content: []realtimeproto.ContentPart{{Type: "input_image", ImageJPEG: image}}}})
		}
		_ = session.Send(ctx, realtimeproto.ResponseCreate{})
		var spoken strings.Builder
		var firstAudio time.Duration
		for done := false; !done; {
			select {
			case event := <-session.Events():
				switch e := event.(type) {
				case realtimeproto.ResponseOutputAudioDelta:
					if firstAudio == 0 {
						firstAudio = time.Since(started)
					}
					spoken.Write(e.Audio.Data)
				case realtimeproto.ResponseDone:
					done = true
				}
			case <-ctx.Done():
				t.Fatal("超时")
			}
		}
		t.Logf("你：%s\n    haro（首句 %s）：%s", text, firstAudio.Round(time.Millisecond), spoken.String())
	}
}

// testChatModel：HARO_LLM_PROVIDER=cloud 且填了 key 时走云端（OpenAI 兼容接口），否则走本机 ollama。
// ⚠️ 走云端时若设了 HARO_TEST_IMAGE，画面会发到云端 —— 只在用户同意时设置。
func testChatModel(t *testing.T) brain.ChatModel {
	client := &http.Client{Timeout: 2 * time.Minute}
	if os.Getenv("HARO_LLM_PROVIDER") == "cloud" && os.Getenv("HARO_CLOUD_LLM_API_KEY") != "" {
		t.Logf("模型：云端 %s", os.Getenv("HARO_CLOUD_LLM_MODEL"))
		return llm.NewOpenAIChatModel(os.Getenv("HARO_CLOUD_LLM_URL"), os.Getenv("HARO_CLOUD_LLM_MODEL"),
			os.Getenv("HARO_CLOUD_LLM_API_KEY"), 300, client)
	}
	t.Log("模型：本地 qwen3-vl:4b-instruct")
	return llm.NewOllamaChatModel("http://127.0.0.1:11434", "qwen3-vl:4b-instruct", 4096, "30m", client)
}
