package brain

import (
	"encoding/json"
	"fmt"
)

// 一句话里模型连着调用工具的步数和死循环（第 6 段 6.2）：每次请求模型算一步；到最后一步时工具定义照给、
// 但不许调用，逼模型用文字收尾。同一个工具、同样的参数，在最近几步里反复出现就当成死循环：不再执行，
// 告诉模型为什么停，下一步不许再调用工具。阈值是数值判断，放在 Options 里。

const (
	DefaultMaxToolSteps     = 8 // 前台一句话最多请求模型几次（最后一次不许调用工具）
	DefaultToolRepeatLimit  = 3 // 同一个调用（工具 + 参数）出现第几次算死循环
	DefaultToolRepeatWindow = 4 // 只看最近几步里的
)

// loopStoppedNote 是不执行重复调用时交给模型的结果。
const loopStoppedNote = `{"status":"stopped","note":"同一个工具、同样的参数已经调用了 %d 次，结果不会变，这次没有执行。不要再调用工具，用已经拿到的结果直接回答主人；没查到就如实说。"}`

// toolSteps 记一句话里每一步的调用，判断死循环。
type toolSteps struct {
	window, limit int
	steps         [][]string // 每一步里各调用的签名（工具名 + 规范化的参数）
}

func newToolSteps(opts Options) *toolSteps {
	window, limit := opts.ToolRepeatWindow, opts.ToolRepeatLimit
	if window <= 0 {
		window = DefaultToolRepeatWindow
	}
	if limit <= 0 {
		limit = DefaultToolRepeatLimit
	}
	return &toolSteps{window: window, limit: limit}
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
	last := len(t.steps) - 1
	t.steps[last] = append(t.steps[last], signature)
	return count, false
}

// maxToolSteps 是这一句最多请求模型几次。
func (s *Session) maxToolSteps() int {
	if s.opts.MaxToolSteps > 0 {
		return s.opts.MaxToolSteps
	}
	return DefaultMaxToolSteps
}

func loopStoppedResult(count int) string { return fmt.Sprintf(loopStoppedNote, count) }
