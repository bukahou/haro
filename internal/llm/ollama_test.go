package llm

import (
	"testing"

	"haro/internal/brain"
)

func TestUserImageIsSplitIntoItsOwnMessage(t *testing.T) {
	converted := toOllamaMessages([]brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "系统"},
		{Role: brain.RoleUser, Text: "上一句"},
		{Role: brain.RoleAssistant, Text: "上一个回答"},
		{Role: brain.RoleUser, Text: "现在几点了？", Image: []byte("jpeg")},
	})
	if len(converted) != 5 {
		t.Fatalf("应多出一条画面消息，实际 %d 条", len(converted))
	}
	frame, question := converted[3], converted[4]
	if frame.Role != "user" || frame.Content != cameraFrameNote || len(frame.Images) != 1 {
		t.Errorf("画面消息 %+v", frame)
	}
	if question.Content != "现在几点了？" || len(question.Images) != 0 {
		t.Errorf("问题消息不应再带图片：%+v", question)
	}
}
