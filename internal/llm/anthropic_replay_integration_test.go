//go:build integration

// 开着思考时思考块原样带回，对真实 Claude API 的检查（默认不跑；约 5 次请求，开着思考，不到 2 日元）：
//
//	HARO_ANTHROPIC_API_KEY=… go test -tags integration -run AnthropicLiveThinkingReplay -v -count=1 ./internal/llm/
//
// 请求一律打开最严的检查（thinking.block_binding.prefix_mismatch_behavior = error：思考块之前的内容但凡变了，整个请求 400），
// 看一个工具来回几次的调查式对话能不能一路被接受。阳性对照：改掉思考块之前的一句话再带回，应被拒。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// strictThinkingTransport 给每个请求打开最严的思考块检查（测试版功能：带上对应的 beta 头）。
type strictThinkingTransport struct{ base http.RoundTripper }

func (s strictThinkingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body == nil {
		return s.base.RoundTrip(request)
	}
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if thinking, ok := body["thinking"].(map[string]any); ok && thinking["type"] == ThinkingAdaptive {
		thinking["block_binding"] = map[string]any{"prefix_mismatch_behavior": "error"}
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	clone := request.Clone(request.Context())
	clone.Body, clone.ContentLength = io.NopCloser(bytes.NewReader(patched)), int64(len(patched))
	clone.Header.Add("anthropic-beta", "thinking-binding-controls-2026-08-01")
	return s.base.RoundTrip(clone)
}

func TestAnthropicLiveThinkingReplay(t *testing.T) {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	modelName := os.Getenv("HARO_TEST_CLAUDE_MODEL")
	if modelName == "" {
		modelName = "claude-haiku-5-5"
	}
	client := &http.Client{Transport: strictThinkingTransport{base: http.DefaultTransport}}
	model := NewAnthropicChatModel(AnthropicOptions{API: claudeAPIForTest(key, "", client), Model: modelName, MaxTokens: 8000,
		Effort: "high", Thinking: ThinkingAdaptive})
	tools := []brain.ToolSpec{{Name: "get_population", Description: "查一个城市的人口（一次只查一个城市）",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}
	populations := map[string]string{"东京": "1,416 万", "大阪": "277 万"}
	question := "先查东京的人口，看到结果后再查大阪的人口（一次只查一个），最后告诉我东京比大阪多多少。"
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: "你是调查助手，用中文简短回答；需要数字时用工具查，不凭记忆。"},
		{Role: brain.RoleUser, Text: question}}

	ask := func(label string, request []brain.ChatMessage) (brain.Reply, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		started := time.Now()
		reply, err := brain.CollectReply(ctx, model, brain.ChatRequest{Messages: request, Tools: tools})
		t.Logf("%s：%v，文字 %q，调用 %d 个，带回内容 %v，输出 %d token，错误 %v", label, time.Since(started).Round(time.Millisecond),
			reply.Text, len(reply.ToolCalls), reply.Replay != nil, reply.Usage.OutputTokens, err)
		return reply, err
	}

	replays, sentWithReplay := 0, 0
	var firstAnswer *brain.ChatMessage // 阳性对照用：第一次带着思考块的回答
	for step := 1; step <= 5; step++ {
		for _, message := range messages {
			if message.Replay != nil {
				sentWithReplay++
				break
			}
		}
		reply, err := ask(fmt.Sprintf("第 %d 次", step), messages)
		if err != nil {
			t.Fatalf("第 %d 次请求被拒（思考块带回不合格？）：%v", step, err)
		}
		answer := brain.ChatMessage{Role: brain.RoleAssistant, Text: reply.Text, ToolCalls: reply.ToolCalls, Replay: reply.Replay}
		if reply.Replay != nil {
			replays++
			if firstAnswer == nil && len(reply.ToolCalls) > 0 {
				firstAnswer = &answer
			}
		}
		messages = append(messages, answer)
		if len(reply.ToolCalls) == 0 {
			if !strings.Contains(reply.Text, "万") {
				t.Errorf("最后应给出人口的比较：%q", reply.Text)
			}
			break
		}
		for _, call := range reply.ToolCalls {
			city, _ := call.Arguments["city"].(string)
			result, found := populations[strings.TrimSuffix(strings.TrimSuffix(city, "都"), "市")]
			if !found {
				result = "没有这个城市的数据"
			}
			messages = append(messages, brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
		}
	}
	if replays == 0 || sentWithReplay == 0 || firstAnswer == nil {
		t.Fatalf("应有带着思考块的回答，并带回去再请求过：回答 %d 次带思考块，带回 %d 次", replays, sentWithReplay)
	}

	// 阳性对照：思考块之前的那句问题改了，思考块照样带回：最严的检查应拒绝（不拒说明检查没生效，上面的「被接受」不算数）
	tampered := []brain.ChatMessage{messages[0], {Role: brain.RoleUser, Text: strings.Replace(question, "大阪", "京都", 1)}, *firstAnswer}
	for _, call := range firstAnswer.ToolCalls {
		tampered = append(tampered, brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: "1,416 万"})
	}
	if _, err := ask("阳性对照（改了前面的话）", tampered); err == nil {
		t.Error("改了思考块之前的内容还被接受：最严的检查没生效")
	}
}
