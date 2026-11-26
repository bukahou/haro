package e2e

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
	"github.com/bukahou/haro/internal/tools"
)

// 长期记忆的整条路径：声纹认出主人的一句 → 前台调 remember → 工具箱（经 MCP）→ 记在主人名下；
// 声纹没认出的一句（沿用上一句的人）调 remember 不记（工具报错交回模型）。常驻的那条下一轮附在情况说明里。
func TestRememberThroughSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	store, err := notes.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keeper := memory.NewKeeper(store, nil, nil, 20, logger)
	box, err := tools.NewToolbox(ctx, logger, tools.MemoryTools(keeper, time.UTC, logger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	model := braintest.NewModel(t,
		braintest.Call(tools.RememberTool, map[string]any{"content": "不吃香菜", "source": "model", "resident": true}), braintest.Say("记住了。"),
		braintest.Call(tools.RememberTool, map[string]any{"content": "车停在 C3", "source": "owner"}), braintest.Say("这句没认出是你。"),
		braintest.Say("好的。"))
	voice := braintest.NewVoice()
	session := brain.NewSession(brain.Components{Streaming: voice, Front: brain.Agent{Name: "前台", Model: model, MaxSteps: 8},
		Synthesizer: echoSynthesizer{}, Tools: box, Briefers: []brain.Briefer{keeper},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, logger)
	defer session.Close()
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{})

	braintest.Done(t, braintest.Ask(t, session, stream, braintest.Utterance{Text: "我不吃香菜", Speaker: "主人", SpeakerChecked: true}))
	braintest.Done(t, braintest.Ask(t, session, stream, braintest.Utterance{Text: "记一下车停在 C3", CheckFailed: true}))
	list, total, err := store.List(ctx, "主人", 10)
	if err != nil || total != 1 || list[0].Content != "不吃香菜" || list[0].Person != "主人" || list[0].Source != notes.SourceModel {
		t.Fatalf("声纹认出的那句记在主人名下，没认出的不记：%+v %v", list, err)
	}

	braintest.Done(t, braintest.Ask(t, session, stream, braintest.Utterance{Text: "今天吃什么", Speaker: "主人", SpeakerChecked: true}))
	last := model.LastRequest().Messages
	found := false
	for _, message := range last {
		found = found || (message.Synthetic && strings.Contains(message.Text, "不吃香菜（#"))
	}
	if !found {
		t.Error("常驻的那条应附在下一轮的情况说明里")
	}
}
