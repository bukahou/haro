package brain

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// 滚动摘要（v5 R4 b）：只留最近 MaxHistoryTurn 轮会忘事；掉出窗口的对话攒够一批，
// 在后台压成一小段摘要，之后每轮放进系统提示词。成本几乎不涨：只在有对话掉出窗口时多一次小调用。
// 还没压进摘要的那几轮照常带着（不会出现「刚掉出去、摘要还没写」的空档）。

const (
	summaryBatch    = 3                // 掉出窗口的对话攒够几轮压一次
	summaryMaxRunes = 600              // 摘要最多多少字（超过就截，免得越滚越长）
	summaryTimeout  = 60 * time.Second // 一次压缩最多等多久
)

// SummarizeWith 开启滚动摘要：model 用来压缩（已按 summary 用途记账）。不调用 = 掉出窗口的对话直接丢掉（旧行为）。
func (b *ConversationBook) SummarizeWith(model ChatModel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.summaryModel = model
}

func (b *ConversationBook) summarizer() ChatModel {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.summaryModel
}

// maybeSummarize 在掉出窗口的对话攒够一批、且这份对话没有正在压缩时，在后台压一次。
func (b *ConversationBook) maybeSummarize(c *Conversation) {
	model := b.summarizer()
	if model == nil {
		return
	}
	c.mu.Lock()
	if c.summarizing || len(c.unsummarized) < summaryBatch {
		c.mu.Unlock()
		return
	}
	c.summarizing = true
	previous, batch := c.summary, append([]ConversationTurn(nil), c.unsummarized...)
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), summaryTimeout)
		defer cancel()
		summary, err := summarizeTurns(ctx, model, previous, batch)
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

// summarizeTurns 把旧摘要和掉出窗口的几轮压成新摘要（只看用户与助手的话，不看工具往来；每轮带上时间）。
func summarizeTurns(ctx context.Context, model ChatModel, previous string, turns []ConversationTurn) (string, error) {
	var transcript strings.Builder
	for _, turn := range turns {
		transcript.WriteString(turnTimeLabel(turn.At) + "\n")
		for _, message := range turn.Messages {
			switch {
			case message.Role == RoleUser && message.Text != "" && !message.Synthetic:
				transcript.WriteString("主人：" + message.Text + "\n")
			case message.Role == RoleAssistant && message.Text != "":
				transcript.WriteString("助手：" + message.Text + "\n")
			}
		}
	}
	if previous == "" {
		previous = "（还没有）"
	}
	messages := []ChatMessage{
		{Role: RoleSystem, Text: "你负责给语音助手维护「之前聊过什么」的摘要。把旧摘要和新的几段对话合成一段新摘要：" +
			"保留主人说过的事实、偏好、约定、没聊完的话题和重要结论；去掉寒暄和已经不重要的细节。" +
			"只写对话里明确出现过的内容：不要推测主人的情绪、意图或和助手的关系，不要评价对话氛围。" +
			"用第三人称、中文、不超过 300 字，只输出摘要本身。"},
		{Role: RoleUser, Text: "旧摘要：\n" + previous + "\n\n新的对话：\n" + transcript.String()},
	}
	reply, err := CollectReply(ctx, model, ChatRequest{Messages: messages})
	if err != nil { // 包括写到一半到了输出上限：半份摘要不用，留着旧摘要和没压进去的对话，下次再压
		return "", err
	}
	text := strings.TrimSpace(reply.Text)
	if utf8.RuneCountInString(text) > summaryMaxRunes {
		text = string([]rune(text)[:summaryMaxRunes])
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
