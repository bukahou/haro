//go:build integration

// 真实模型 + 内置工具的端到端对话（默认不跑；语音用文字代替）：
//
//	HARO_ANTHROPIC_API_KEY=… [HARO_TEST_IMAGE=/path/frame.jpg] go test -tags integration -run ConversationLive -v ./internal/tools/
package tools

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

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
	// 与正式运行一致：长期记忆内置（这里不按意思找：不用嵌入服务）
	builtins := []Builtin{MemoryTools(memory.NewKeeper(store, nil, nil, 200, logger), time.Local, logger)}
	box, err := NewToolbox(ctx, logger, builtins...)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	voice := braintest.NewVoice()
	parts := brain.Components{
		Streaming: voice, Synthesizer: textSynthesizer{}, Tools: box,
		Front:        builtinFront(t, testChatModel(t)),
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}
	if image, _ := os.ReadFile(os.Getenv("HARO_TEST_IMAGE")); len(image) > 0 { // 终端的摄像头每次拍到这张
		parts.Camera = braintest.NewCamera(image)
	}
	session := brain.NewSession(parts, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	defer session.Close()
	turns := []string{"帮我记一下，车停在C3的7号位。", "我的车停在哪了？", "现在几点了？",
		"我最近都记了些什么？", "把车停车位那条记录删了。", "确认", "你能看到我吗？"}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{})
	if conversation := os.Getenv("HARO_TEST_CONVERSATION"); conversation != "" {
		turns = strings.Split(conversation, "|")
	}

	for _, text := range turns {
		utterance := braintest.Utterance{Text: text}
		braintest.Speak(t, session, stream, utterance)
		started := time.Now()
		braintest.Expect[realtimeproto.ResponseCreated](t, session)
		braintest.Recognized(t, stream, utterance)
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

// testChatModel 是这个测试用的模型：Claude（HARO_ANTHROPIC_API_KEY；模型名 HARO_CLOUD_LLM_MODEL，不设用 Haiku）。没设密钥时跳过。
// ⚠️ 设了 HARO_TEST_IMAGE 时画面会发到云端 —— 只在用户同意时设置。
func testChatModel(t *testing.T) brain.ChatModel {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	model := os.Getenv("HARO_CLOUD_LLM_MODEL")
	if model == "" {
		model = "claude-haiku-5-5"
	}
	t.Logf("模型：%s", model)
	api, err := llm.NewClaudeAPI(key, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return llm.NewAnthropicChatModel(llm.AnthropicOptions{API: api, Model: model, MaxTokens: 800})
}

// builtinFront 是按程序里带的定义组的前台（真模型）。
func builtinFront(t *testing.T, model brain.ChatModel) brain.Agent {
	t.Helper()
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	front, _ := catalog.Get(agents.Front)
	instructions, err := front.RenderPrompt(brain.PromptValues())
	if err != nil {
		t.Fatal(err)
	}
	return brain.Agent{Name: front.Title, Model: model, Instructions: instructions, MaxSteps: front.MaxSteps,
		RepeatLimit: front.RepeatLimit, RepeatWindow: front.RepeatWindow}
}
