package chat

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/brain"
)

// TitleMaxRunes 是标题最多几个字（起标题的代理按它写，超过的截掉）。
const TitleMaxRunes = 20

// TitlePromptValues 是起标题的代理正文里引用的值（{{.TitleChars}}）。
func TitlePromptValues() map[string]any { return map[string]any{"TitleChars": TitleMaxRunes} }

// Titler 给一段聊天起标题。
type Titler interface {
	Title(ctx context.Context, said, answer string) (string, error)
}

// AgentTitler 用代理「起标题」起标题。
type AgentTitler struct{ Agent brain.Agent }

// Title 实现 Titler。
func (t AgentTitler) Title(ctx context.Context, said, answer string) (string, error) {
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: t.Agent.Instructions},
		{Role: brain.RoleUser, Text: "主人：" + said + "\nharo：" + answer}}
	reply, err := brain.CollectReply(ctx, t.Agent.Model, brain.ChatRequest{Messages: messages})
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(reply.Text)
	if title == "" {
		return "", errors.New("起标题的代理没写标题")
	}
	if utf8.RuneCountInString(title) > TitleMaxRunes {
		title = string([]rune(title)[:TitleMaxRunes])
	}
	return title, nil
}
