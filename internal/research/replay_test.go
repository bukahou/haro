package research

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// withReplay 给一步回答带上要原样带回的内容（开着思考时适配层交出的；内容由测试指定）。
func withReplay(delta brain.ReplyDelta, data string) brain.ReplyDelta {
	delta.Replay = &brain.ProviderReplay{Provider: "test", Data: json.RawMessage(strconv.Quote(data))}
	return delta
}

// replaysIn 是一次请求里各条回答带着的 Replay 的内容（按顺序）。
func replaysIn(messages []brain.ChatMessage) []string {
	var replays []string
	for _, message := range messages {
		if message.Replay != nil {
			replays = append(replays, string(message.Replay.Data))
		}
	}
	return replays
}

// 调查的模型每一步的 Replay 跟着回答带到下一步（搜索阶段工具清单不变）；到上限写结论的那一次只给交结论的工具，
// 工具清单变了，一个都不带。
func TestResearchCarriesReplayWithinTask(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{
		withReplay(call("search_web", map[string]any{"query": "A B 价格"}), "r1"),
		withReplay(call("read_pages", map[string]any{"urls": []any{"https://example.com/a"}}), "r2"),
	}}
	rig := newRig(t, model, rigSetup{rounds: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)
	task, _, err := rig.runner.Start(ctx, "主人", "A 和 B 哪个便宜")
	if err != nil {
		t.Fatal(err)
	}
	if done := rig.waitFinished(t, task.ID); done.Status != StatusPartial || done.Conclusion != limitConclusion {
		t.Fatalf("到轮数上限应用查到的写结论：%+v", done)
	}
	requests := model.requestMessages()
	if len(requests) != 3 {
		t.Fatalf("应请求 3 次（搜、读、写结论），实际 %d 次", len(requests))
	}
	if got := replaysIn(requests[1]); !slices.Equal(got, []string{`"r1"`}) {
		t.Errorf("第二步应带着第一步的 Replay：%v", got)
	}
	if got := replaysIn(requests[2]); len(got) != 0 {
		t.Errorf("写结论那次换了工具清单，不该带 Replay：%v", got)
	}
}
