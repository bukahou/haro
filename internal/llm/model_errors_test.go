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
