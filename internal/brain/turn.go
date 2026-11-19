package brain

import "strings"

// 回合收尾：一轮里的进展随时记进 turnRecord（主人的话、每次工具往来和结果、最后说的话），
// 不管这一轮怎么结束（说完、被打断、模型出错），都由 closeTurn 整理成一组完整的消息写进历史：
//   - 模型要求的每个工具调用都有结果：做完的照实记，没做的补「没有执行」（历史里没有落单的工具调用）；
//   - 已经做完的工具（例如记事已经存了）即使之后被打断、出错，也留在历史里，模型下一轮知道自己做过；
//   - 调用工具前说的话记在那条工具调用的消息里；最后一条回答只记最后一段，不重复；被打断时只记听到的部分。

// turnEnding 是一轮怎么结束的。
type turnEnding int

const (
	turnCompleted   turnEnding = iota // 说完了
	turnInterrupted                   // 被打断（主人又说话、终端取消）
	turnFailed                        // 模型或合成出错
)

// turnRecord 是这一轮到目前为止的进展。只在回答的那个 goroutine 里写，收尾时读。
type turnRecord struct {
	question string      // 主人的原话（不含每轮附带的情况说明）
	rounds   []toolRound // 调用了工具的那几段（按顺序）
	// final 是最后一段（不再调用工具时）说的话。段号：第 i 段工具往来是第 i 段，最后一段的段号 = 工具往来的段数
	// （和说出口的句子上的段号对上：被打断时只算最后一段里听到的）
	final strings.Builder
	// askedConfirmation：这一轮问了主人「确定要删掉吗」（被打断时撤掉这个待确认的操作）
	askedConfirmation bool
	// delivered：这一轮附上的、要「说完才算告诉了」的情况的确认（任务板的结论、额度提醒）；只在说完时调用
	delivered []func()
}

// toolRound 是模型一次要求调用工具：调用前说的话、要求的调用、各自的结果。
type toolRound struct {
	text       string
	calls      []ToolCall
	results    map[string]string // 按调用编号；没有 = 没执行完
	remembered map[string]string // 历史里换一种说法的结果（只对这一轮有用的要求不存进历史，换成中性的记录）
	frameShown bool              // 这一段把摄像头画面直接交给了模型（历史里只留一句「当时看了画面」）
}

func newTurnRecord(question string) *turnRecord { return &turnRecord{question: question} }

// finalRound 是最后一段的段号（之前每段工具往来各占一个段号）。
func (r *turnRecord) finalRound() int { return len(r.rounds) }

// beginRound 记下模型要求的一批工具调用，返回这一段的编号（结果用 recordResult 填）。
func (r *turnRecord) beginRound(text string, calls []ToolCall) int {
	r.rounds = append(r.rounds, toolRound{text: text, calls: calls, results: make(map[string]string, len(calls))})
	return len(r.rounds) - 1
}

func (r *turnRecord) recordResult(round int, call ToolCall, result string) {
	r.rounds[round].results[call.ID] = result
}

// rememberAs 让这个调用的结果在历史里记成 text（交给模型的仍是 recordResult 那份）。
func (r *turnRecord) rememberAs(round int, call ToolCall, text string) {
	if r.rounds[round].remembered == nil {
		r.rounds[round].remembered = map[string]string{}
	}
	r.rounds[round].remembered[call.ID] = text
}

// executedTools 是这一轮做完了几个工具调用。
func (r *turnRecord) executedTools() int {
	n := 0
	for _, round := range r.rounds {
		n += len(round.results)
	}
	return n
}

// 收尾时补的话（给模型看的历史，不念给主人）。
const (
	toolNotRunInterrupted = "（这一轮被主人打断，这个调用没有执行）"
	toolNotRunFailed      = "（这一轮出错了，这个调用没有执行）"
	failedBeforeSpeaking  = "（这一轮出错了，没有说出回答）"
	nothingSaid           = "（这一轮没有说话）"
)

// messages 把这一轮整理成写进历史的一组消息：主人的话 → 每段工具往来（每个调用都有结果）→ 最后的回答。
func (r *turnRecord) messages(ending turnEnding, answer string) []ChatMessage {
	notRun := toolNotRunInterrupted
	if ending == turnFailed {
		notRun = toolNotRunFailed
	}
	turn := []ChatMessage{{Role: RoleUser, Text: r.question}}
	for _, round := range r.rounds {
		turn = append(turn, ChatMessage{Role: RoleAssistant, Text: round.text, ToolCalls: round.calls})
		for _, call := range round.calls {
			result, ok := round.results[call.ID]
			switch remembered, replaced := round.remembered[call.ID]; {
			case !ok:
				result = notRun
			case replaced:
				result = remembered
			}
			turn = append(turn, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
		}
		if round.frameShown {
			turn = append(turn, ChatMessage{Role: RoleUser, Text: frameRecalledNote, Synthetic: true})
		}
	}
	return append(turn, ChatMessage{Role: RoleAssistant, Text: answer})
}

// closeTurn 收尾：按这一轮怎么结束整理出最后的回答，连同工具往来一起写进历史。
// 什么都没发生（没做任何工具、终端没报播到哪、也没说出一句）的被打断 / 出错的一轮不记。返回记没记。
func (s *Session) closeTurn(record *turnRecord, ending turnEnding) bool {
	s.mu.Lock()
	var answer string
	switch ending {
	case turnCompleted:
		answer = strings.TrimSpace(record.final.String())
		if answer == "" {
			answer = nothingSaid
		}
	case turnInterrupted:
		if s.truncateAtMs >= 0 { // 终端报了播到哪：一句没听到也记（模型要知道自己被打断了）
			answer = heardText(s.spoken, s.truncateAtMs, record.finalRound())
			if answer == "" {
				answer = interruptedBeforeSpeaking
			}
		}
	case turnFailed:
		answer = spokenText(s.spoken, record.finalRound())
	}
	s.answerRound = record.finalRound()
	item := s.responseItem
	s.mu.Unlock()

	if ending != turnCompleted && answer == "" && record.executedTools() == 0 { // 什么都没发生（例如没播就取消了）
		return false
	}
	switch {
	case ending == turnInterrupted && answer == "":
		answer = interruptedBeforeSpeaking
	case ending == turnFailed && answer == "":
		answer = failedBeforeSpeaking
	}
	if ending == turnInterrupted && record.askedConfirmation { // 问「确定要删掉吗」时被打断：这件事作废
		s.takePendingConfirmation()
		s.logger.Info("问确认时被打断：撤掉待确认的操作")
	}
	s.rememberTurn(record.messages(ending, answer))
	if ending == turnCompleted { // 说完了才算把附上的结论、提醒告诉了主人；被打断、出错时下一轮再附上
		for _, delivered := range record.delivered {
			delivered()
		}
	}
	if ending != turnCompleted {
		s.logger.Info("这一轮没说完：照实记进历史", "item", item, "ending", endingLabel(ending),
			"tools_done", record.executedTools(), "answer", answer)
	}
	return true
}

func endingLabel(ending turnEnding) string {
	switch ending {
	case turnInterrupted:
		return "被打断"
	case turnFailed:
		return "出错"
	default:
		return "说完"
	}
}
