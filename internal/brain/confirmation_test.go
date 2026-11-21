package brain_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// owner 让登记过声纹的主人说一句（确认闸只认这样的话）。
func (k *talker) owner(text string) []realtimeproto.ServerEvent {
	k.t.Helper()
	return braintest.Ask(k.t, k.session, k.stream, braintest.Utterance{Text: text, Speaker: "主人", SpeakerChecked: true})
}

// toolResultOf 解出这次请求里最后一条工具结果（确认闸交给模型的 JSON）。
func toolResultOf(t *testing.T, request braintest.Request) map[string]any {
	t.Helper()
	last := request.Last()
	var result map[string]any
	if last.Role != brain.RoleTool || json.Unmarshal([]byte(last.Text), &result) != nil {
		t.Fatalf("最后一条应是确认闸的结果（JSON）：%+v", last)
	}
	return result
}

var deleteNoteThree = braintest.Call("delete_note", map[string]any{"ids": []any{3}})

func newGateTalker(t *testing.T, model brain.ChatModel, toolbox brain.Toolbox, timeout time.Duration) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Tools: toolbox},
		brain.Options{MaxHistoryTurn: 6, ConfirmationTimeout: timeout})
}

// 同意：会破坏的调用先存下、要点交给模型去问；主人回答后，算不算同意由模型判断（测试直接指定它调用 confirm_action），
// 用当初存下的参数执行。有会破坏的工具时确认、取消的定义一直带着：每次请求的工具清单都一样（提示词缓存不失效）。
func TestConfirmationGateExecutesStoredCallOnConfirm(t *testing.T) {
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("要删掉第 3 条吗？"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("删好了。"),
		braintest.Say("好的。"))
	toolbox := &fakeToolbox{}
	k := newGateTalker(t, model, toolbox, 0)

	if spoken := braintest.Spoken(k.owner("把那条删了")); spoken != "要删掉第 3 条吗？" {
		t.Fatalf("应由模型用自己的话问：%q", spoken)
	}
	if len(toolbox.called()) != 0 {
		t.Fatal("确认之前不应执行")
	}
	held := toolResultOf(t, model.Requests()[1])
	if held["status"] != "needs_confirmation" || held["action_id"] != "act_1" || held["tool"] != "delete_note" || held["arguments"] == nil {
		t.Errorf("交给模型的应是待确认的要点（来自参数）：%v", held)
	}

	k.owner("嗯，删吧")
	if names := model.Requests()[0].ToolNames(); !slices.Contains(names, "confirm_action") || !slices.Contains(names, "cancel_action") {
		t.Errorf("有会破坏的工具时，第一轮就带着确认、取消两个工具：%v", names)
	}
	if got := toolbox.called(); !slices.Equal(got, []string{"delete_note"}) {
		t.Fatalf("确认后应执行：%v", got)
	}
	if ids := toolbox.argumentsOf(0)["ids"]; !slices.Equal(ids.([]any), []any{3}) {
		t.Errorf("应按当初存下的参数执行：%v", ids)
	}
	if done := toolResultOf(t, model.Requests()[3]); done["status"] != "done" || done["result"] != `{"deleted":true}` {
		t.Errorf("执行结果应交回模型：%v", done)
	}

	k.owner("谢谢")
	for i, request := range model.Requests() {
		if names := request.ToolNames(); !slices.Equal(names, model.Requests()[0].ToolNames()) {
			t.Errorf("第 %d 次请求的工具清单变了（提示词缓存会失效）：%v", i+1, names)
		}
	}
}

// 拒绝：模型调用 cancel_action，作废、不执行；作废之后再确认那个编号也不执行。
func TestConfirmationGateCancel(t *testing.T) {
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("要删掉第 3 条吗？"),
		braintest.Call("cancel_action", map[string]any{"action_id": "act_1"}), braintest.Say("好，不删了。"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("已经取消了。"))
	toolbox := &fakeToolbox{}
	k := newGateTalker(t, model, toolbox, 0)
	k.owner("把那条删了")
	k.owner("还是别删了")
	if len(toolbox.called()) != 0 {
		t.Errorf("取消了不应执行：%v", toolbox.called())
	}
	if result := toolResultOf(t, model.Requests()[3]); result["status"] != "cancelled" {
		t.Errorf("取消的结果：%v", result)
	}
	k.owner("嗯，删吧")
	if result := toolResultOf(t, model.Requests()[5]); result["status"] != "refused" || len(toolbox.called()) != 0 {
		t.Errorf("作废了的编号再确认也不执行：%v %v", result, toolbox.called())
	}
}

// 过期：问了之后超过时效才确认，不执行，告诉模型已经过期。
func TestConfirmationGateExpires(t *testing.T) {
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("要删掉第 3 条吗？"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("已经过期了。"))
	toolbox := &fakeToolbox{}
	k := newGateTalker(t, model, toolbox, time.Nanosecond)
	k.owner("把那条删了")
	k.owner("可以")
	if len(toolbox.called()) != 0 {
		t.Errorf("过期了不应执行：%v", toolbox.called())
	}
	if result := toolResultOf(t, model.Requests()[3]); result["status"] != "refused" {
		t.Errorf("过期的确认应被拒：%v", result)
	}
}

// 不是登记过的人说的（没认出声纹）：会破坏的操作不做，也不存下等确认（之后主人确认那个编号也没有可执行的）。
func TestConfirmationGateDeniesUnverifiedVoice(t *testing.T) {
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("这个我不能帮你删。"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("好的。"))
	toolbox := &fakeToolbox{}
	k := newGateTalker(t, model, toolbox, 0)
	k.say("把那条删了")
	if result := toolResultOf(t, model.Requests()[1]); result["status"] != "denied" {
		t.Errorf("没认出声纹：应直接不做：%v", result)
	}
	k.owner("嗯，删吧")
	if result := toolResultOf(t, model.Requests()[3]); result["status"] != "refused" || len(toolbox.called()) != 0 {
		t.Errorf("不应存下待确认的操作、不应执行：%v %v", result, toolbox.called())
	}
}

// 同一轮就确认（主人还没回答）：不执行。
func TestConfirmationGateRefusesSameTurn(t *testing.T) {
	model := braintest.NewModel(t, deleteNoteThree, braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}),
		braintest.Say("要删掉第 3 条吗？"))
	toolbox := &fakeToolbox{}
	k := newGateTalker(t, model, toolbox, 0)
	k.owner("把那条删了")
	if len(toolbox.called()) != 0 {
		t.Errorf("同一轮自己确认不应执行：%v", toolbox.called())
	}
	if result := toolResultOf(t, model.Requests()[2]); result["status"] != "refused" {
		t.Errorf("同一轮的确认应被拒：%v", result)
	}
}

// 确认时塞了别的参数：不认（不按模型这次给的执行）；之后好好确认，执行的仍是当初存下的参数。
func TestConfirmationGateIgnoresChangedArguments(t *testing.T) {
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("要删掉第 3 条吗？"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1", "ids": []any{9}}), braintest.Say("嗯？"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("删好了。"))
	toolbox := &fakeToolbox{}
	k := newGateTalker(t, model, toolbox, 0)
	k.owner("把那条删了")
	k.owner("删第 9 条吧")
	if len(toolbox.called()) != 0 {
		t.Fatalf("带了别的参数的确认不应执行：%v", toolbox.called())
	}
	k.owner("就删刚才那条")
	if got := toolbox.called(); len(got) != 1 || !slices.Equal(toolbox.argumentsOf(0)["ids"].([]any), []any{3}) {
		t.Errorf("执行的应是当初存下的参数：%v %v", got, toolbox.arguments)
	}
}
