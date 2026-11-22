package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/llm/llmtest"
)

// 连不上（过载、认证失败、连接被拒、流中途断开）标成 brain.ErrModelUnavailable；请求本身有问题、主人打断都不算。
func TestContractUnavailableIsTyped(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		request := brain.ChatRequest{Messages: contractQuestion}
		for _, c := range []struct {
			name        string
			status      int
			unavailable bool
		}{{"过载", 529, true}, {"认证失败", 401, true}, {"限流", 429, true}, {"请求有问题", 400, false}} {
			server := vendor.newServer(t, llmtest.Script{Status: c.status})
			reply := collectContract(t.Context(), vendor.newModel(server), request, nil)
			if reply.err == nil || errors.Is(reply.err, brain.ErrModelUnavailable) != c.unavailable {
				t.Errorf("%s（%d）：算不算连不上应为 %v：%v", c.name, c.status, c.unavailable, reply.err)
			}
		}

		gone := vendor.newServer(t)
		model := vendor.newModel(gone)
		gone.Close() // 连接被拒
		if reply := collectContract(t.Context(), model, request, nil); !errors.Is(reply.err, brain.ErrModelUnavailable) {
			t.Errorf("连接被拒应算连不上：%v", reply.err)
		}

		cut := vendor.newServer(t, llmtest.Script{Events: vendor.events([]string{"从前", "有座山"}, nil, contractDone), CutAfter: vendor.firstText + 1})
		if reply := collectContract(t.Context(), vendor.newModel(cut), request, nil); !errors.Is(reply.err, brain.ErrModelUnavailable) {
			t.Errorf("流中途断开应算连不上：%v", reply.err)
		}

		// 已经回了 200、开始推流之后服务端才发来错误事件（过载）：照样算连不上；之前说了的照样交出，用量标不完整
		started := vendor.events([]string{"从前"}, nil, contractDone)[:vendor.firstText+1]
		overloaded := vendor.newServer(t, llmtest.Script{Events: append(started, vendor.overloaded)})
		if reply := collectContract(t.Context(), vendor.newModel(overloaded), request, nil); !errors.Is(reply.err, brain.ErrModelUnavailable) ||
			reply.text() != "从前" || reply.usage == nil || !reply.usage.Incomplete {
			t.Errorf("流开始之后发来的过载应算连不上：%+v", reply)
		}

		hanging := vendor.newServer(t, llmtest.Script{Events: vendor.events([]string{"从前"}, nil, contractDone)[:vendor.firstText+1], Hang: true})
		ctx, cancel := context.WithCancel(t.Context())
		reply := collectContract(ctx, vendor.newModel(hanging), request, cancel)
		cancel()
		if !errors.Is(reply.err, context.Canceled) || errors.Is(reply.err, brain.ErrModelUnavailable) {
			t.Errorf("主人打断不算连不上：%v", reply.err)
		}
		// 调用方自己的期限到了（例如后台调查的总时限）：也不算服务的问题
		deadline, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer stop()
		reply = collectContract(deadline, vendor.newModel(hanging), request, nil)
		if reply.err == nil || errors.Is(reply.err, brain.ErrModelUnavailable) {
			t.Errorf("调用方的期限到了不算连不上：%v", reply.err)
		}
	})
}

// 流开始之后才来的错误事件按错误码认，不看说明文字：Claude 看错误类型（请求本身有问题的不算连不上）；
// OpenAI 兼容接口错误体带数字状态码的看状态码，不带的（OpenAI 写的是字符串类型）算连不上。
func TestStreamErrorEventsAreClassifiedByCode(t *testing.T) {
	request := brain.ChatRequest{Messages: contractQuestion}
	claude, openai := contractVendors[0], contractVendors[1]
	claudeStart := claudeEvents(nil, nil, contractDone)[0]
	openAIText := openAIEvents([]string{"从前"}, nil, contractDone)[0]
	for _, c := range []struct {
		name        string
		vendor      contractVendor
		events      []string
		unavailable bool
	}{
		{"Claude 过载", claude, []string{claudeStart, `{"type":"error","error":{"type":"overloaded_error","message":"bad request"}}`}, true},
		{"Claude 服务端出错", claude, []string{claudeStart, `{"type":"error","error":{"type":"api_error","message":"bad request"}}`}, true},
		{"Claude 限流", claude, []string{claudeStart, `{"type":"error","error":{"type":"rate_limit_error","message":"bad request"}}`}, true},
		{"Claude 请求有问题", claude, []string{claudeStart, `{"type":"error","error":{"type":"invalid_request_error","message":"Overloaded"}}`}, false},
		{"OpenAI 兼容 数字 503", openai, []string{openAIText, `{"error":{"message":"bad request","code":503}}`}, true},
		{"OpenAI 兼容 数字 400", openai, []string{openAIText, `{"error":{"message":"Overloaded","code":400}}`}, false},
		{"OpenAI 兼容 字符串类型", openai, []string{openAIText, `{"error":{"message":"bad request","type":"server_error"}}`}, true},
	} {
		server := c.vendor.newServer(t, llmtest.Script{Events: c.events})
		reply := collectContract(t.Context(), c.vendor.newModel(server), request, nil)
		if reply.err == nil || errors.Is(reply.err, brain.ErrModelUnavailable) != c.unavailable {
			t.Errorf("%s：算不算连不上应为 %v：%v", c.name, c.unavailable, reply.err)
		}
	}
}
