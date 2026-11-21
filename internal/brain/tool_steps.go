package brain

import (
	"encoding/json"
	"fmt"
	"slices"
)

// 一句话里模型连着调用工具的步数和死循环：每次请求模型算一步；到最后一步时工具定义照给、
// 但不许调用，逼模型用文字收尾。同一个工具、同样的参数，在最近几步里反复出现就当成死循环：不再执行，
// 告诉模型为什么停，下一步不许再调用工具。阈值是数值判断，每个代理在自己的定义里写（Agent）。

// stepNote 是没执行的调用交给模型的结果：状态 + 为什么没执行、接下来怎么做。
type stepNote struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

func (n stepNote) String() string {
	data, _ := json.Marshal(n)
	return string(data)
}

// loopStoppedResult 是不执行重复调用时交给模型的结果。
func loopStoppedResult(count int) string {
	return stepNote{Status: "stopped", Note: fmt.Sprintf("同一个工具、同样的参数已经调用了 %d 次，结果不会变，这次没有执行。"+
		"不要再调用工具，用已经拿到的结果直接回答主人；没查到就如实说。", count)}.String()
}

// notOfferedResult 是调用了这一步没提供的工具时交给模型的结果。
func notOfferedResult(tool string) string {
	return stepNote{Status: "not_offered", Note: "这一步没有提供 " + tool + " 这个工具，没有执行。只用这一步提供的工具，或者直接回答主人。"}.String()
}

// executable 判断这一步能不能执行这个调用（执行方自己守门，不假设模型只调用给了它的工具）：要是这一步提供了的。
// 确认、取消例外：没有待确认的操作、或刚问过主人时这一步不提供它们，但调用了也交给它们自己判断 ——
// 它们本身就是闸（核对编号、时效、是不是下一句、声纹），会说明为什么不执行（过期了、要等主人回答），
// 模型才能照实告诉主人。代理的允许名单里没有它们、或这一轮不给工具时照样不执行。
func executable(agent Agent, offered []ToolSpec, restricted bool, tool string) bool {
	if slices.ContainsFunc(offered, func(spec ToolSpec) bool { return spec.Name == tool }) {
		return true
	}
	return !restricted && agent.allows(tool) && isGateTool(tool)
}

// toolSteps 记一句话里每一步的调用，判断死循环。
type toolSteps struct {
	window, limit int
	steps         [][]string // 每一步里各调用的签名（工具名 + 规范化的参数）
}

// newToolSteps 按代理的阈值开始记；RepeatLimit 为 0 时不查死循环。
func newToolSteps(agent Agent) *toolSteps {
	return &toolSteps{window: agent.RepeatWindow, limit: agent.RepeatLimit}
}

// callSignature 是一个调用的签名：工具名 + 参数（JSON 编码时键按字母排，同样的参数得到同样的签名）。
func callSignature(call ToolCall) string {
	arguments, _ := json.Marshal(call.Arguments)
	return call.Name + " " + string(arguments)
}

// begin 开始新的一步。
func (t *toolSteps) begin() { t.steps = append(t.steps, nil) }

// repeated 判断这个调用加上最近几步（含这一步之前的调用）里一样的，是不是到了死循环的次数；没到就记下它。
func (t *toolSteps) repeated(call ToolCall) (int, bool) {
	signature := callSignature(call)
	last := len(t.steps) - 1
	if t.limit <= 0 {
		t.steps[last] = append(t.steps[last], signature)
		return 1, false
	}
	count := 1
	for _, step := range t.steps[max(0, len(t.steps)-t.window):] {
		for _, seen := range step {
			if seen == signature {
				count++
			}
		}
	}
	if count >= t.limit {
		return count, true
	}
	t.steps[last] = append(t.steps[last], signature)
	return count, false
}
