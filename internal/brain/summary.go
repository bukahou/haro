package brain

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// 滚动摘要（v5 R4 b）：只留最近 MaxHistoryTurn 轮会忘事；掉出窗口的对话攒够一批，
// 在后台交给代理「摘要」（子任务）压成一小段摘要，之后每轮放进系统提示词。成本几乎不涨：只在有对话掉出窗口时多一次小调用。
// 还没压进摘要的那几轮照常带着（不会出现「刚掉出去、摘要还没写」的空档）。怎么写摘要在它的定义里（系统提示词）。

const (
	summaryBatch = 3 // 掉出窗口的对话攒够几轮压一次
	// summaryChars 是摘要的字数上限：经 SummaryPromptValues 写进代理「摘要」的提示词（{{.SummaryChars}}）。
	// 写到它的 summaryHardFactor 倍以上时不截断（会截在句子中间），请它压短一次；还超就不用，留着旧摘要下次再压，免得越滚越长
	summaryChars      = 300
	summaryHardFactor = 2
)

// 交给代理「摘要」的对话里，主人和助手说的话前面的标签。
const (
	summaryOwnerLabel     = "主人："
	summaryAssistantLabel = "助手："
)

// SummaryPromptValues 是代理「摘要」的提示词里引用的值。
func SummaryPromptValues() map[string]any { return map[string]any{"SummaryChars": summaryChars} }

// SummarizeWith 开启滚动摘要：交给 agent 压缩（它的模型已按自己的用途记账）。不调用 = 掉出窗口的对话直接丢掉（旧行为）。
func (b *ConversationBook) SummarizeWith(agent Agent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.summaryAgent = &agent
}

func (b *ConversationBook) summarizer() *Agent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.summaryAgent
}

// maybeSummarize 在掉出窗口的对话攒够一批、且这份对话没有正在压缩时，在后台压一次。
func (b *ConversationBook) maybeSummarize(c *Conversation) {
	agent := b.summarizer()
	if agent == nil {
		return
	}
	c.mu.Lock()
	if c.summarizing || len(c.unsummarized) == 0 || (len(c.unsummarized) < summaryBatch && !c.compacted) {
		c.mu.Unlock()
		return
	}
	c.summarizing, c.compacted = true, false
	previous, batch := c.summary, append([]ConversationTurn(nil), c.unsummarized...)
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		if agent.Timeout > 0 { // 一次压缩最多等多久（定义里的 timeout）
			ctx, cancel = context.WithTimeout(context.Background(), agent.Timeout)
		}
		defer cancel()
		summary, err := summarizeTurns(ctx, b.logger, *agent, previous, batch)
		c.mu.Lock()
		c.summarizing = false
		if err == nil && summary != "" {
			c.summary = summary
			c.unsummarized = c.unsummarized[len(batch):] // 压缩期间又掉出来的留到下一批
		}
		c.mu.Unlock()
		if err != nil || summary == "" {
			b.logger.Warn("对话摘要没写成（下次再试；这几轮先照常带着）", "person", c.person, "err", err)
			return
		}
		b.logger.Info("对话摘要已更新", "person", c.person, "turns", len(batch), "chars", utf8.RuneCountInString(summary))
		b.save(c)
	}()
}

// summarizeTurns 把旧摘要和掉出窗口的几轮交给代理「摘要」压成新摘要（只看用户与助手的话，不看工具往来；每轮带上时间）。
func summarizeTurns(ctx context.Context, logger *slog.Logger, agent Agent, previous string, turns []ConversationTurn) (string, error) {
	ctx, err := EnterAgent(ctx, logger, agent.Name, Subtask)
	if err != nil {
		return "", err
	}
	var transcript strings.Builder
	for _, turn := range turns {
		transcript.WriteString(turnTimeLabel(turn.At) + "\n")
		for _, message := range turn.Messages {
			switch {
			case message.Role == RoleUser && message.Text != "" && !message.Synthetic:
				transcript.WriteString(summaryOwnerLabel + message.Text + "\n")
			case message.Role == RoleAssistant && message.Text != "":
				transcript.WriteString(summaryAssistantLabel + message.Text + "\n")
			}
		}
	}
	if previous == "" {
		previous = "（还没有）"
	}
	messages := []ChatMessage{
		{Role: RoleSystem, Text: agent.Instructions},
		{Role: RoleUser, Text: "旧摘要：\n" + previous + "\n\n新的对话：\n" + transcript.String()},
	}
	reply, err := CollectReply(ctx, agent.Model, ChatRequest{Messages: messages})
	if err != nil { // 包括写到一半到了输出上限：半份摘要不用，留着旧摘要和没压进去的对话，下次再压
		return "", err
	}
	text := strings.TrimSpace(reply.Text)
	if utf8.RuneCountInString(text) <= summaryChars*summaryHardFactor {
		return text, nil
	}
	messages = append(messages, ChatMessage{Role: RoleAssistant, Text: text},
		ChatMessage{Role: RoleUser, Text: fmt.Sprintf("太长了：压到 %d 字以内，只输出摘要本身。", summaryChars)})
	if reply, err = CollectReply(ctx, agent.Model, ChatRequest{Messages: messages}); err != nil {
		return "", err
	}
	text = strings.TrimSpace(reply.Text)
	if n := utf8.RuneCountInString(text); n > summaryChars*summaryHardFactor {
		return "", fmt.Errorf("摘要压短后还有 %d 字，超过上限 %d 字的 %d 倍", n, summaryChars, summaryHardFactor)
	}
	return text, nil
}

// summaryNote 是放进系统提示词的摘要段落。
func summaryNote(summary string) string {
	if summary == "" {
		return ""
	}
	return "【之前聊过的（摘要，比下面的对话更早）】\n" + summary
}
