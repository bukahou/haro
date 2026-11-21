package llm

import (
	"iter"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// collect 把一次流式回答收齐：文字、工具调用、最后带的用量；出错时返回已经收到的部分和错误。
func collect(t *testing.T, seq iter.Seq2[brain.ReplyDelta, error]) (string, []brain.ToolCall, *brain.ReplyUsage, error) {
	t.Helper()
	var text strings.Builder
	var calls []brain.ToolCall
	var usage *brain.ReplyUsage
	for delta, err := range seq {
		if delta.Usage != nil {
			usage = delta.Usage
		}
		if err != nil {
			return text.String(), calls, usage, err
		}
		text.WriteString(delta.Text)
		calls = append(calls, delta.ToolCalls...)
	}
	return text.String(), calls, usage, nil
}
