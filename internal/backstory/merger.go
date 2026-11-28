package backstory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
)

// saveTool 是代理交回新清单用的工具（只在这里定义一处；正文里用 {{.SaveTool}} 引用）。
const saveTool = "save_backstory"

// PromptValues 是代理「背景记录池」正文里引用的值。
func PromptValues() map[string]any { return map[string]any{"MaxChars": MaxRunes, "SaveTool": saveTool} }

type saveInput struct {
	Items []savedItem `json:"items" jsonschema:"新的背景清单（后台整理的部分，全部交回；主人改过的那几条不用交）"`
}

type savedItem struct {
	Text     string `json:"text" jsonschema:"一条背景，一句话"`
	FromTurn int64  `json:"from_turn,omitempty" jsonschema:"这条主要来自哪一轮（对话里标的轮次编号）；整理旧条目、说不清时不填"`
}

var saveSpec = brain.ToolSpec{Name: saveTool, Parameters: brain.ParametersFor[saveInput](),
	Description: "交回整理好的背景清单。整理完一定要调用一次，把全部条目交回（不是只交新增的）。"}

// AgentMerger 用代理「背景记录池」合并、整理。
type AgentMerger struct{ Agent brain.Agent }

// Merge 实现 Merger：一次交给代理，它用 save_backstory 交回新清单；超过 MaxRunes 时再请它压短一次，仍然超的照存（提示词里只放得下的那些）。
func (m AgentMerger) Merge(ctx context.Context, kept, current []Entry, turns []conversationstore.FoundTurn) ([]Entry, error) {
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: m.Agent.Instructions},
		{Role: brain.RoleUser, Text: describe(kept, current, turns)}}
	entries, err := m.ask(ctx, messages)
	if err != nil {
		return nil, err
	}
	if total(kept)+total(entries) <= MaxRunes {
		return entries, nil
	}
	messages = append(messages, brain.ChatMessage{Role: brain.RoleUser,
		Text: fmt.Sprintf("太长了：连主人改过的一共要在 %d 字以内。合并、删掉不重要的，再调用一次 %s。", MaxRunes, saveTool)})
	if shorter, err := m.ask(ctx, messages); err == nil {
		return shorter, nil
	}
	return entries, nil
}

// ask 请代理整理一次，取它用工具交回的清单。
func (m AgentMerger) ask(ctx context.Context, messages []brain.ChatMessage) ([]Entry, error) {
	reply, err := brain.CollectReply(ctx, m.Agent.Model, brain.ChatRequest{Messages: messages, Tools: []brain.ToolSpec{saveSpec}})
	if err != nil {
		return nil, err
	}
	for _, call := range reply.ToolCalls {
		if call.Name != saveTool { // 执行方自己守门：只认给了它的工具
			continue
		}
		input, err := brain.DecodeArguments[saveInput](call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("代理交回的清单不对: %w", err)
		}
		entries := make([]Entry, 0, len(input.Items))
		for _, item := range input.Items {
			entries = append(entries, Entry{Text: item.Text, SourceSeq: item.FromTurn})
		}
		return entries, nil
	}
	return nil, errors.New("代理没有交回清单（没调用 " + saveTool + "）")
}

// describe 是交给代理的材料：主人改过的（只读）、现在后台整理的、新的几轮对话（带轮次编号）。
func describe(kept, current []Entry, turns []conversationstore.FoundTurn) string {
	var text strings.Builder
	section := func(title string, entries []Entry) {
		text.WriteString(title + "\n")
		if len(entries) == 0 {
			text.WriteString("（没有）\n")
		}
		for _, entry := range entries {
			text.WriteString("- " + entry.Text + "\n")
		}
		text.WriteString("\n")
	}
	section("主人改过的（只读：不要改、不要重复、不用交回）：", kept)
	section("现在的背景清单（后台整理的）：", current)
	if len(turns) == 0 {
		text.WriteString("这次没有新的对话：只整理上面的清单（合并重复的、去掉过时的）。\n")
		return text.String()
	}
	text.WriteString("新的对话（按轮次）：\n")
	for _, turn := range turns {
		text.WriteString("第 " + strconv.FormatInt(turn.Seq, 10) + " 轮")
		if !turn.At.IsZero() {
			text.WriteString("（" + turn.At.Local().Format("2006-01-02 15:04") + "）")
		}
		text.WriteString("\n")
		for _, message := range turn.Messages {
			switch {
			case message.Role == brain.RoleUser && !message.Synthetic && message.Text != "":
				text.WriteString("主人：" + message.Text + "\n")
			case message.Role == brain.RoleAssistant && message.Text != "":
				text.WriteString("haro：" + message.Text + "\n")
			}
		}
	}
	return text.String()
}

func total(entries []Entry) int {
	sum := 0
	for _, entry := range entries {
		sum += utf8.RuneCountInString(entry.Text)
	}
	return sum
}
