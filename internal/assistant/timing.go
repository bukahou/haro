package assistant

import (
	"cmp"
	"time"
)

// Timing 是终端对话循环里固定的几个时长，集中在这里，每个字段写明是怎么来的。
// 能配置的时长（播完后的回声保护、对话窗口）在 Options 里，来自终端的配置。
type Timing struct {
	// CancelConfirm：被打断后等服务端确认取消（response.done）最多这么久，等不到就不等了（期间收到的事件留给主循环）。
	// 定值：服务端收到取消后马上结束这一轮，这个上限只防它一直不回
	CancelConfirm time.Duration
	// ToneSlack：播一段提示音最多等「提示音的时长 + 这么久」（打开设备、等播完），超过就停掉，不让主循环一直卡着。定值
	ToneSlack time.Duration
}

// DefaultTiming 是现在在用的值（来历见 Timing 的各字段）。
var DefaultTiming = Timing{CancelConfirm: 3 * time.Second, ToneSlack: 5 * time.Second}

// orDefault 把没设（零值）的字段换成 DefaultTiming 的。
func (t Timing) orDefault() Timing {
	t.CancelConfirm = cmp.Or(t.CancelConfirm, DefaultTiming.CancelConfirm)
	t.ToneSlack = cmp.Or(t.ToneSlack, DefaultTiming.ToneSlack)
	return t
}
