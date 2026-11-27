package brain

import (
	"strings"
	"time"
)

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
	turnRestarted                     // 服务端在这一轮中途重启（只用来预先存一份，重启后接进历史）
	turnTruncated                     // 回答到了模型的输出上限，没说完
)

// turnRecord 是这一轮到目前为止的进展。只在回答的那个 goroutine 里写，收尾时读。
type turnRecord struct {
	turn      int64     // 这一轮的编号（确认闸：确认必须在发起之后的一轮）
	startedAt time.Time // 主人这句话的时间（历史里每一轮标的时间）
	// voiceVerified：这句话的声纹认出了登记过的人（确认闸只让这样的人发起、确认会破坏的操作）
	voiceVerified bool
	// endConversation：模型调用了 end_conversation（主人要结束这次对话）：这一轮说完，终端播完就休眠
	endConversation bool
	question        string      // 主人的原话（不含每轮附带的情况说明）
	rounds          []toolRound // 调用了工具的那几段（按顺序）
	// final 是最后一段（不再调用工具时）说的话。段号：第 i 段工具往来是第 i 段，最后一段的段号 = 工具往来的段数
	// （和说出口的句子上的段号对上：被打断时只算最后一段里听到的）
	final strings.Builder
	// askedConfirmation：这一轮存下了待确认的操作、要问主人（问话没说完就被打断时撤掉）
	askedConfirmation bool
	// restricted：声纹没确认的叫醒短句，受限回答：不给工具，这一轮不记进历史
	restricted bool
	// truncated：最后一段到了模型的输出上限，没说完
	truncated bool
	// capRaisedTo：这一轮主人确认后把月度上限提高到了多少（日元）；0 = 没提高
	capRaisedTo float64
	// contextTokens：这一轮请求模型时带的上下文——最大的那次请求的输入 token（模型报的；不知道时为 0）
	contextTokens int
	// contextUnknown：这一轮有说完了的请求没报输入用量（模型服务不报）：按 token 压缩对这一轮不起作用
	contextUnknown bool
	// extraNotes：这一轮情况说明里额外的附注（例如叫醒后的第一句）
	extraNotes []string
	// delivered：要「主人听完这一轮才算告诉了」的事——附上的情况的确认（任务板、额度提醒）和工具登记的（OnDelivered）；
	// 只在说完、播放没被打断时调用
	delivered []func()
	// interrupted：这一轮附上的情况被打断时要通知的（Briefing.Interrupted）
	interrupted []func()
}

// toolRound 是模型一次要求调用工具：调用前说的话、要求的调用、各自的结果。
type toolRound struct {
	text       string
	calls      []ToolCall
	results    map[string]string // 按调用编号；没有 = 没执行完
	remembered map[string]string // 历史里换一种说法的结果（只对这一轮有用的要求不存进历史，换成中性的记录）
	frameShown bool              // 这一段把摄像头画面直接交给了模型（历史里只留一句「当时看了画面」）
}

func newTurnRecord(question string) *turnRecord {
	return &turnRecord{turn: turnCounter.Add(1), startedAt: time.Now(), question: question}
}

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

// Add 实现 Deliveries：工具登记的「主人听完才算数」的事，和附上的情况一起在主人听完这一轮之后调用。
func (r *turnRecord) Add(delivered func()) { r.delivered = append(r.delivered, delivered) }

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
	toolNotRunRestarted   = "（服务端在这一轮中途重启了，这个调用没有执行）"
	failedBeforeSpeaking  = "（这一轮出错了，没有说出回答）"
	restartedUnfinished   = "（服务端在这一轮中途重启了，没有说完）"
	nothingSaid           = "（这一轮没有说话）"
	answerTruncated       = "（回答到了输出上限，后面的没有说出来）"
)

// messages 把这一轮整理成写进历史的一组消息：主人的话 → 每段工具往来（每个调用都有结果）→ 最后的回答。
func (r *turnRecord) messages(ending turnEnding, answer string) []ChatMessage {
	notRun := toolNotRunInterrupted
	switch ending {
	case turnFailed:
		notRun = toolNotRunFailed
	case turnRestarted:
		notRun = toolNotRunRestarted
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
	if record.restricted { // 声纹没确认：不记进任何人的历史
		return false
	}
	s.mu.Lock()
	var answer string
	switch ending {
	case turnCompleted:
		answer = strings.TrimSpace(record.final.String())
		if answer == "" {
			answer = nothingSaid
		}
	case turnTruncated:
		answer = strings.TrimSpace(strings.TrimSpace(record.final.String()) + answerTruncated)
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
	if ending == turnInterrupted && record.askedConfirmation { // 问确认时被打断：主人没听完问话，这一轮存下的作废
		dropped := s.currentConversation().dropPendingActionsFrom(record.turn)
		s.logger.Info("问确认时被打断：撤掉这一轮存下的待确认操作", "dropped", dropped)
	}
	s.rememberTurn(record.startedAt, record.messages(ending, answer), record.contextTokens, record.contextUnknown)
	cut := ending == turnInterrupted
	if ending == turnCompleted && (len(record.delivered) > 0 || len(record.interrupted) > 0) { // 被打断、出错时不算告诉了，下一轮再附上
		s.mu.Lock()
		if s.truncateAtMs < 0 { // 说完了，还要等主人听完（见 pendingDelivered）
			s.pendingDelivered, s.pendingInterrupted = record.delivered, record.interrupted
		} else { // 生成完之前就被打断在播放里了
			cut = true
		}
		s.mu.Unlock()
	}
	if cut {
		for _, interrupted := range record.interrupted {
			interrupted()
		}
	}
	if ending != turnCompleted {
		s.logger.Info("这一轮没说完：照实记进历史", "item", item, "ending", endingLabel(ending),
			"tools_done", record.executedTools(), "answer", answer)
	}
	return true
}

// keepOpenTurn 这一轮做完了一个工具：先按「服务端重启、没说完」的样子存一份。之后正常收尾时被替换掉；
// 万一服务端在这一轮中途重启，下次读到它照实接进历史（做过的工具不会丢）。
func (s *Session) keepOpenTurn(record *turnRecord) {
	s.currentConversation().keepOpenTurn(ConversationTurn{At: record.startedAt,
		Messages: record.messages(turnRestarted, restartedUnfinished)})
}

func endingLabel(ending turnEnding) string {
	switch ending {
	case turnInterrupted:
		return "被打断"
	case turnFailed:
		return "出错"
	case turnTruncated:
		return "到了输出上限"
	default:
		return "说完"
	}
}
