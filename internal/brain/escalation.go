package brain

import "encoding/json"

// askStrongerTool 是内置的「请更强的模型重答」工具：主人在纠正、或明说要想清楚时由前台模型调用。
// 认「主人在纠正」交给模型：任何语言、任何说法都认得出，不靠写死的词表。
// 调用后这一轮剩下的请求标上 Escalate，由选模型的包装层交给更强的模型（Options.Escalation 为 true 时才提供）。
const askStrongerTool = "ask_stronger_model"

var askStrongerSpec = ToolSpec{Name: askStrongerTool,
	Description: "把主人这一句交给更强的模型重新回答。主人说你答错了、不是这个、让你重新想，或者明说要你想清楚、换个更强的来答时，" +
		"一定调用：不要自己争辩或坚持原来的答案（你判断不了自己错在哪）。调用前最多说一句「我再想想」，不要提交给别的模型的事。",
	Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}

const (
	// 这条结果之后的请求已经交给更强的模型：话是对接手的模型说的
	escalatedNote        = "这一句已改由更强的模型重新回答，现在回答的就是你：认真重新想（需要就查），直接回答主人；不要提交接的事，也不要再调用这个工具。"
	alreadyEscalatedNote = "这一句已经由更强的模型在回答了，就是你：直接回答主人。"
	// escalatedRecord 是历史里这次交接记成的样子：上面两句是给接手的模型这一轮看的要求，原样存下来，
	// 之后的轮次会把「不要再调用这个工具」当成规矩，主人再纠正时就不交接了
	escalatedRecord = "（这一句交给了更强的模型回答）"
)

// escalate 把这一轮主人那句话标上 Escalate（input 指向 messages 里的那一条），返回交给模型的工具结果。
func (s *Session) escalate(input *ChatMessage) string {
	if input.Escalate {
		return alreadyEscalatedNote
	}
	input.Escalate = true
	s.logger.Info("调用工具", "tool", askStrongerTool)
	return escalatedNote
}
