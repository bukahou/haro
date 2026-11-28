package brain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 一轮里发生的事，给 App 的打字对话显示：工具在做什么、派了后台调查、要主人确认的操作。
// 终端协议里没有这些（终端不显示），只有打字会话设了 Components.Activity 时才发；语音会话不受影响。
// 卡片（调查、确认）还记在历史里那次工具结果上（ChatMessage.Card），重新打开对话时照样显示。

// ActivityObserver 收一轮里发生的事。在回答的那个 goroutine 里调用：不要做慢的事。
type ActivityObserver interface {
	ToolStarted(callID, label string) // 开始执行一个工具；label 是给人看的说明（按这一轮的语言）
	ToolFinished(callID string)       // 那个工具做完了（成没成由模型在回答里说）
	CardShown(card Card)              // 聊天里要出一张卡片
}

// CardKind 是卡片的种类。
type CardKind string

const (
	CardResearch     CardKind = "research"     // 派了一个后台调查
	CardConfirmation CardKind = "confirmation" // 一次要主人确认的操作（会删掉或改掉东西）
)

// Card 是聊天里的一张卡片。
type Card struct {
	Kind      CardKind  `json:"kind"`
	TaskID    int64     `json:"task_id,omitempty"`  // research：调查的编号
	Question  string    `json:"question,omitempty"` // research：调查的问题
	ActionID  string    `json:"action_id,omitempty"`
	Tool      string    `json:"tool,omitempty"` // confirmation：要执行哪个工具
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

type cardSinkKey struct{}

// withCardSink 让这次工具调用里报告的卡片交给 show（记进这一轮、发给 App）。
func withCardSink(ctx context.Context, show func(Card)) context.Context {
	return context.WithValue(ctx, cardSinkKey{}, show)
}

// ShowCard 由工具在执行时调用：聊天里出一张卡片（例如派了调查）。只有打字会话收；别处调用时什么都不做。
func ShowCard(ctx context.Context, card Card) {
	if show, ok := ctx.Value(cardSinkKey{}).(func(Card)); ok {
		show(card)
	}
}

// ShowsCards 判断这次工具调用的结果会不会在聊天里显示成卡片（打字会话）：工具据此决定怎么告诉模型跟主人说。
func ShowsCards(ctx context.Context) bool {
	_, ok := ctx.Value(cardSinkKey{}).(func(Card))
	return ok
}

// ToolLabelKey 是固定的话里工具说明的键（tool.<工具名>）；toolLabelFallback 是没配说明的工具用的。
func ToolLabelKey(tool string) string { return "tool." + tool }

const toolLabelFallback = "tool"

// unfilledPlaceholder 匹配说明里没填上的参数（例如工具没给 query 时的「：{query}」）：连同前面的冒号一起去掉。
var unfilledPlaceholder = regexp.MustCompile(`\s*[:：]?\s*\{[a-z_]+\}`)

// ToolLabel 是一次工具调用给人看的说明（按 language）：固定的话里配了 tool.<工具名> 就用它，参数里的文字填进 {参数名}；
// 没配时用工具自己声明的标题（外部 MCP 服务给的），再没有就是通用的一句。
func (p Phrases) ToolLabel(language string, call ToolCall, title string) string {
	if _, ok := p.Languages[language]; !ok {
		language = p.Default
	}
	if _, ok := p.Languages[language][ToolLabelKey(call.Name)]; !ok {
		if title != "" {
			return title
		}
		return p.Say(language, toolLabelFallback, nil)
	}
	values := map[string]string{}
	for name, value := range call.Arguments {
		switch v := value.(type) {
		case string:
			values[name] = v
		case float64, int, int64:
			values[name] = fmt.Sprint(v)
		}
	}
	text := p.Say(language, ToolLabelKey(call.Name), values)
	return strings.TrimSpace(unfilledPlaceholder.ReplaceAllString(text, ""))
}

// Decision 是主人在确认卡片上点的：确认还是取消哪一次操作。
type Decision struct {
	ActionID string
	Confirm  bool
}

// ActionOpen 判断这个人这条对话里的这次操作还在等确认（没过期、没确认也没取消）：App 显示历史里的确认卡片时用。
func (b *ConversationBook) ActionOpen(person, thread, actionID string) bool {
	action, ok := b.ConversationIn(person, thread).pendingAction(actionID)
	return ok && time.Now().Before(action.ExpiresAt)
}

// toolStarted 在执行一次调用前：打字会话里告诉 App 在做什么，并让这次调用报告的卡片记进这一轮、发给 App。
// 返回执行这次调用用的 ctx（不是打字会话时原样返回）。
func (s *Session) toolStarted(ctx context.Context, record *turnRecord, round int, call ToolCall) context.Context {
	activity := s.parts.Activity
	if activity == nil {
		return ctx
	}
	spec, _ := s.specOf(call.Name)
	activity.ToolStarted(call.ID, s.opts.Phrases.ToolLabel(s.currentLanguage(), call, spec.Title))
	return withCardSink(ctx, func(card Card) {
		record.attachCard(round, call, card)
		activity.CardShown(card)
	})
}

// currentLanguage 是主人最近一句话的语种（固定的话、给人看的说明按它选语言）。
func (s *Session) currentLanguage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.language
}
