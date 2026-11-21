package brain_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// situationOf 是这次请求里情况说明（系统附注）的文字。
func situationOf(request braintest.Request) string {
	var notes []string
	for _, message := range request.Messages {
		if message.Synthetic {
			notes = append(notes, message.Text)
		}
	}
	return strings.Join(notes, "\n")
}

// 叫醒后的第一句、打断时说的：整句原样交给模型（唤醒词不删），情况说明里告诉它开头的唤醒词、回声不是主人要说的。
func TestWakeUtteranceGoesToTheModelWithANote(t *testing.T) {
	model := braintest.NewModel(t, braintest.Say("我在。"), braintest.Say("好，停下了。"), braintest.Say("嗯。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}}, brain.Options{MaxHistoryTurn: 6})

	if spoken := braintest.Spoken(k.owner2(braintest.Utterance{Wake: true, Text: "哈罗哈罗。"})); spoken != "我在。" {
		t.Errorf("只叫了唤醒词：由模型应一声：%q", spoken)
	}
	first := model.Requests()[0]
	if first.Input().Text != "哈罗哈罗。" || !strings.Contains(situationOf(first), fmt.Sprintf(brain.WakeNote, "唤醒词")) {
		t.Errorf("整句原样交给模型，附上叫醒后的说明和唤醒词原文：%q / %q", first.Input().Text, situationOf(first))
	}
	k.owner2(braintest.Utterance{Wake: true, BargeIn: true, Text: "我觉得你应该哈罗哈罗，停一下"})
	if note := situationOf(model.Requests()[1]); !strings.Contains(note, "打断你说的") || !strings.Contains(note, "回声") {
		t.Errorf("打断时说的：附上打断的说明：%q", note)
	}
	k.owner2(braintest.Utterance{Text: "今天怎么样"})
	if note := situationOf(model.Requests()[2]); strings.Contains(note, "叫醒") || strings.Contains(note, "打断") {
		t.Errorf("不是叫醒后的第一句：不该有这条说明：%q", note)
	}
}

// 声纹只过了唤醒的门槛、没确认：叫醒的短句受限回答（不给工具、不带个人资料和历史，也不记进历史）；
// 长句、或者不是叫醒的，不回答。
func TestUnverifiedWakeShortUtteranceGetsARestrictedReply(t *testing.T) {
	model := braintest.NewModel(t, braintest.Say("你好呀。"), braintest.Say("我在。"), braintest.Say("在呢。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{},
		Profiles: staticProfiles{"主人": "【主人资料】车停在 C3"}}, brain.Options{MaxHistoryTurn: 6})
	k.owner2(braintest.Utterance{Text: "你好", Speaker: "主人", SpeakerChecked: true}) // 现在接的是主人的对话

	short := braintest.Utterance{Wake: true, Text: "哈罗哈罗", SpeakerChecked: true, Tentative: "主人", Audio: 1500 * time.Millisecond}
	if done := braintest.Done(t, k.owner2(short)); done.Status != realtimeproto.StatusCompleted {
		t.Fatalf("叫醒的短句应受限回答：%+v", done)
	}
	restricted := model.Requests()[1]
	if len(restricted.Tools) != 0 || strings.Contains(restricted.System(), "主人资料") || !strings.Contains(situationOf(restricted), brain.RestrictedNote) {
		t.Errorf("受限回答：不给工具、不带个人资料，并说明声纹没认出：工具 %v / %q / %q", restricted.ToolNames(), restricted.System(), situationOf(restricted))
	}

	for _, u := range []braintest.Utterance{
		{Wake: true, Text: "哈罗哈罗，帮我把记事都删了", SpeakerChecked: true, Tentative: "主人", Audio: 4 * time.Second},
		{Text: "在吗", SpeakerChecked: true, Tentative: "主人", Audio: time.Second},
		{Wake: true, Text: "哈罗哈罗", SpeakerChecked: true, Audio: time.Second},
	} {
		if done := braintest.Done(t, k.owner2(u)); done.Status != realtimeproto.StatusIncomplete || done.Reason != realtimeproto.ReasonUnknownSpeaker {
			t.Errorf("长句、不是叫醒的、连唤醒门槛都没过的：不回答：%+v → %+v", u, done)
		}
	}

	if slices.ContainsFunc(restricted.Messages, func(message brain.ChatMessage) bool { return message.Text == "你好" }) {
		t.Error("受限回答不带历史")
	}
	k.owner2(braintest.Utterance{Text: "我回来了", Speaker: "主人", SpeakerChecked: true})
	last := model.Requests()[2]
	if slices.ContainsFunc(last.Messages, func(message brain.ChatMessage) bool { return message.Text == "哈罗哈罗" }) {
		t.Error("受限回答的那一轮不应记进历史")
	}
}

// owner2 让主人说一句（带上识别给的各项标记）。
func (k *talker) owner2(u braintest.Utterance) []realtimeproto.ServerEvent {
	k.t.Helper()
	return braintest.Ask(k.t, k.session, k.stream, u)
}
