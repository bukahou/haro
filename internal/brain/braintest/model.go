// Package braintest 是测试大脑（internal/brain）用的替身和驱动：按脚本回答的模型、由测试驱动的流式识别，
// 以及「主人说一句话 → 请求回答 → 收齐事件」的辅助函数。只给测试用。
package braintest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// Reporter 是 testing.TB 里替身要用的那部分。替身在会话自己的 goroutine 里被调用，只能报错（Errorf），不能中止测试。
type Reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// Reply 是模型某一次被调用时的回答。各部分按字段顺序吐出：文字片段 → 工具调用 → 用量 → 错误。
type Reply struct {
	Chunks    []string          // 依次吐出的文字片段
	ToolCalls []brain.ToolCall  // 文字之后要求调用的工具
	Usage     *brain.ReplyUsage // 最后报告的用量；nil = 不报
	Err       error             // 吐完以上内容后返回的错误
	Delay     time.Duration     // 每个文字片段之前等多久（测打断、取消用）
	Hang      bool              // 吐完之后不结束，直到被取消（模拟还在生成）
}

// Say 是只吐一段文字的回答。
func Say(text string) Reply { return Reply{Chunks: []string{text}} }

// Call 是只要求调用一个工具的回答。
func Call(name string, arguments map[string]any) Reply {
	return Reply{ToolCalls: []brain.ToolCall{{Name: name, Arguments: arguments}}}
}

// Request 是模型收到的一次请求（消息和工具清单的副本）。
type Request struct {
	Messages []brain.ChatMessage
	Tools    []brain.ToolSpec
}

// ToolNames 是这次请求提供的工具名。
func (r Request) ToolNames() []string {
	names := make([]string, 0, len(r.Tools))
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// Last 是这次请求的最后一条消息。
func (r Request) Last() brain.ChatMessage {
	if len(r.Messages) == 0 {
		return brain.ChatMessage{}
	}
	return r.Messages[len(r.Messages)-1]
}

// System 是系统提示词；没有时为空。
func (r Request) System() string {
	if len(r.Messages) > 0 && r.Messages[0].Role == brain.RoleSystem {
		return r.Messages[0].Text
	}
	return ""
}

// Input 是这一轮主人说的话：从后往前第一条不带图片的用户消息（画面消息排在工具结果之后，不算）。
func (r Request) Input() brain.ChatMessage {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if msg := r.Messages[i]; msg.Role == brain.RoleUser && len(msg.Image) == 0 && !msg.Synthetic {
			return msg
		}
	}
	return brain.ChatMessage{}
}

// ErrScriptExhausted 是脚本用完后再被调用时返回的错误。
var ErrScriptExhausted = errors.New("braintest: 模型的脚本已经用完")

// Model 是按脚本回答的模型（brain.ChatModel）：第 N 次被调用就回第 N 条 Reply，不看消息内容。
// 每次被调用时检查消息里的工具调用和工具结果是否成对；脚本用完还被调用算测试失败。
type Model struct {
	report Reporter

	mu       sync.Mutex
	replies  []Reply
	requests []Request
}

// NewModel 创建按 replies 回答的模型。
func NewModel(report Reporter, replies ...Reply) *Model {
	return &Model{report: report, replies: replies}
}

// Then 在脚本末尾追加回答。
func (m *Model) Then(replies ...Reply) *Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies = append(m.replies, replies...)
	return m
}

// Requests 是到目前为止收到的全部请求。
func (m *Model) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// LastRequest 是最近一次请求；还没有请求时为空。
func (m *Model) LastRequest() Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		return Request{}
	}
	return m.requests[len(m.requests)-1]
}

// Remaining 是脚本里还没用到的回答数。
func (m *Model) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.replies)
}

// Warmup 什么也不做。
func (m *Model) Warmup(context.Context) error { return nil }

// StreamReply 记下请求，按脚本回下一条。
func (m *Model) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.requests = append(m.requests, Request{Messages: slices.Clone(messages), Tools: slices.Clone(tools)})
	n := len(m.requests)
	var reply Reply
	exhausted := len(m.replies) == 0
	if !exhausted {
		reply, m.replies = m.replies[0], m.replies[1:]
	}
	m.mu.Unlock()

	if err := CheckToolPairs(messages); err != nil {
		m.report.Errorf("第 %d 次请求的消息不成对：%v", n, err)
	}
	if exhausted {
		m.report.Errorf("第 %d 次请求时脚本已经用完（多出来的模型请求）", n)
		return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, ErrScriptExhausted) }
	}
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, chunk := range reply.Chunks {
			if reply.Delay > 0 {
				select {
				case <-time.After(reply.Delay):
				case <-ctx.Done():
					yield(brain.ReplyDelta{}, ctx.Err())
					return
				}
			}
			if !yield(brain.ReplyDelta{Text: chunk}, nil) {
				return
			}
		}
		if len(reply.ToolCalls) > 0 && !yield(brain.ReplyDelta{ToolCalls: slices.Clone(reply.ToolCalls)}, nil) {
			return
		}
		if reply.Usage != nil {
			usage := *reply.Usage
			if !yield(brain.ReplyDelta{Usage: &usage}, nil) {
				return
			}
		}
		if reply.Hang {
			<-ctx.Done()
			yield(brain.ReplyDelta{}, ctx.Err())
			return
		}
		if reply.Err != nil {
			yield(brain.ReplyDelta{}, reply.Err)
		}
	}
}

// CheckToolPairs 检查消息里的工具调用和工具结果是否成对：每个调用都有编号；每个工具结果都对得上它前面的一个调用；
// 下一条模型回答或主人的话之前，前面的调用都已经有了结果。
func CheckToolPairs(messages []brain.ChatMessage) error {
	pending := map[string]bool{}
	for i, msg := range messages {
		switch msg.Role {
		case brain.RoleAssistant:
			if len(pending) > 0 {
				return fmt.Errorf("第 %d 条（模型回答）之前，调用 %v 还没有结果", i, slices.Sorted(maps.Keys(pending)))
			}
			for _, call := range msg.ToolCalls {
				if call.ID == "" {
					return fmt.Errorf("第 %d 条里的工具调用 %s 没有编号", i, call.Name)
				}
				pending[call.ID] = true
			}
		case brain.RoleTool:
			if !pending[msg.ToolCallID] {
				return fmt.Errorf("第 %d 条工具结果（%s）对不上任何还没有结果的调用：编号 %q", i, msg.ToolName, msg.ToolCallID)
			}
			delete(pending, msg.ToolCallID)
		case brain.RoleUser:
			if len(pending) > 0 && len(msg.Image) == 0 {
				return fmt.Errorf("第 %d 条（主人的话）之前，调用 %v 还没有结果", i, slices.Sorted(maps.Keys(pending)))
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("最后的调用 %v 还没有结果", slices.Sorted(maps.Keys(pending)))
	}
	return nil
}
