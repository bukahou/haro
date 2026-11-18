package llm

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"github.com/bukahou/haro/internal/brain"
)

// RouterChatModel 按每次请求的内容选模型，规则写在代码里、不让模型自己判断要不要换更强的
// （小模型往往意识不到自己答错了，让它决定的话，该换的时候不换）：
//   - 这次请求里带摄像头画面 → Vision（画面只在本轮输入里，下一轮自然回到默认模型）
//   - 前台模型请更强的模型重答（主人在纠正、或明说要想清楚；本轮输入标了 Escalate）→ Correction
//   - 其余 → Default
//
// Vision / Correction 为 nil 时这类请求也用 Default。
type RouterChatModel struct {
	options RouterOptions
}

// RouterOptions 是 RouterChatModel 的各路模型；名字写进日志。
type RouterOptions struct {
	Default                    brain.ChatModel
	Vision, Correction         brain.ChatModel
	VisionName, CorrectionName string
	Logger                     *slog.Logger
}

// NewRouterChatModel 创建按规则选模型的包装层。
func NewRouterChatModel(options RouterOptions) *RouterChatModel {
	return &RouterChatModel{options: options}
}

// StreamReply 见类型说明。
func (r *RouterChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	model, route, name := r.choose(messages)
	if route != "" {
		r.options.Logger.Info("选模型", "route", route, "model", name)
	}
	return model.StreamReply(ctx, messages, tools)
}

func (r *RouterChatModel) choose(messages []brain.ChatMessage) (brain.ChatModel, string, string) {
	o := r.options
	if o.Vision != nil && carriesImage(messages) {
		return o.Vision, "看画面", o.VisionName
	}
	if o.Correction != nil && escalated(messages) {
		return o.Correction, "重答", o.CorrectionName
	}
	return o.Default, "", ""
}

// carriesImage：这次请求里有画面（画面不进历史，有就是本轮看的）。
func carriesImage(messages []brain.ChatMessage) bool {
	for _, message := range messages {
		if len(message.Image) > 0 {
			return true
		}
	}
	return false
}

// escalated 看本轮主人那句话（最后一条不带画面的用户消息；之后只有工具往来和承载画面的消息）有没有标 Escalate。
func escalated(messages []brain.ChatMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == brain.RoleUser && len(messages[i].Image) == 0 {
			return messages[i].Escalate
		}
	}
	return false
}

// Warmup 预热各路模型（云端的预热是空操作）。
func (r *RouterChatModel) Warmup(ctx context.Context) error {
	errs := []error{r.options.Default.Warmup(ctx)}
	for _, model := range []brain.ChatModel{r.options.Vision, r.options.Correction} {
		if model != nil && model != r.options.Default {
			errs = append(errs, model.Warmup(ctx))
		}
	}
	return errors.Join(errs...)
}
