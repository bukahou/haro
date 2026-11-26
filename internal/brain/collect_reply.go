package brain

import (
	"context"
	"errors"
	"strings"
)

// Reply 是收齐的一次回答（不需要边收边念的用途：摘要、后台调查、看画面的专家）。
type Reply struct {
	Text      string
	ToolCalls []ToolCall
	Usage     ReplyUsage      // 不知道时为零值
	Replay    *ProviderReplay // 要原样带回的内容（见 ProviderReplay；没有时为 nil）
}

// ErrTruncated 表示模型的输出到了上限、被截断：收齐回答的用途按失败处理（写了一半的摘要、结论不能用）。
var ErrTruncated = errors.New("模型的输出到了上限，被截断")

// CollectReply 把一次回答收齐。出错时返回已经收到的部分和错误；被截断时返回已经收到的文字和 ErrTruncated；
// 流结束了却没说为什么停时返回已经收到的文字和 ErrStopUnknown（不当成说完）。
func CollectReply(ctx context.Context, model ChatModel, request ChatRequest) (Reply, error) {
	var reply Reply
	var text strings.Builder
	stop := StopUnknown // 为什么停：最后一段说了算
	for delta, err := range model.StreamReply(ctx, request) {
		if delta.Usage != nil {
			reply.Usage = *delta.Usage
		}
		if err != nil {
			reply.Text = text.String()
			return reply, err
		}
		text.WriteString(delta.Text)
		reply.ToolCalls = append(reply.ToolCalls, delta.ToolCalls...)
		if delta.Replay != nil {
			reply.Replay = delta.Replay
		}
		stop = delta.Stop
	}
	reply.Text = text.String()
	switch stop {
	case StopMaxTokens:
		return reply, ErrTruncated
	case StopUnknown:
		return reply, ErrStopUnknown
	}
	return reply, nil
}
