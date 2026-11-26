package brain

import "sync"

// 附上的情况被打断（11-23 真机验收 F9）：一轮回答被主人打断，这一轮附上的情况不算告诉了，下一轮照样再附上。
// 主人一直打断时就会一遍遍附上同一件事（模型每轮都去说它，把主人这次说的话岔开）。所以按编号记被打断了几次：
// 被打断后最多再附上一次，第二次附上又被打断就当告诉过了（查完的调查、错过的提醒这类，主人随时还能问）。

// MaxBriefingCuts：附上同一件事的那一轮被打断到这么多次，就当告诉过了，不再附上。
// 来历：用户 2026-11-26 裁定「被打断最多再附上一次」
const MaxBriefingCuts = 2

// BriefingCuts 按编号记「附上这件事的那一轮被主人打断了」几次。只在内存里：服务端重启后从头算（最多多说一次）。
// 零值可以直接用；可以并发调用。
type BriefingCuts struct {
	mu     sync.Mutex
	counts map[int64]int
}

// Add 记下这几件事又被打断了一次（Briefing.Interrupted 里调用）。
func (c *BriefingCuts) Add(ids []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[int64]int{}
	}
	for _, id := range ids {
		c.counts[id]++
	}
}

// Count 是这件事被打断了几次。
func (c *BriefingCuts) Count(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[id]
}

// Exhausted：这件事被打断到上限了，当告诉过了。
func (c *BriefingCuts) Exhausted(id int64) bool { return c.Count(id) >= MaxBriefingCuts }

// Forget 告诉过了（或不用再附上了）：不再记它。
func (c *BriefingCuts) Forget(ids []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.counts, id)
	}
}

// CutNote 是再附上时接在那一行后面的说明：上次附上时被打断了，由模型看对话历史判断主人是不是已经知道了。
const CutNote = "（上次附上时主人打断了：对话历史里看得出他已经知道了就不用再说。）"
