package brain

import (
	"sync"
	"time"
)

// WakeArbiter 是多台终端同时在线时的唤醒仲裁（v4 D2，Alexa 式）：几台终端在 window 之内都听到唤醒词时，
// 只让唤醒质量最高的那台回答，其余几台回去休眠。只有一台终端在线时不等（不增加延迟，C-2）。
//
// 唤醒质量：正在播放回答的那台被叫（打断自己）最优先；其次比声纹分数，分数接近（差不到 scoreTie）时比音量（离得近的大）。
type WakeArbiter struct {
	window time.Duration

	mu      sync.Mutex
	members map[*Session]struct{} // 在线、开了唤醒词的终端连接
	round   *wakeRound            // 正在收集的一轮（nil = 没有）
}

// scoreTie：声纹分数差不到这么多就算一样，改比音量（短的唤醒词声纹本来就抖，见 v3 BV）。
const scoreTie = 0.05

// WakeClaim 是一台终端的一次唤醒。
type WakeClaim struct {
	Score    float64 // 唤醒词声纹分数
	Level    float64 // 唤醒词那一段的音量
	Barge    bool    // 这台正在播放回答（打断自己）
	terminal *Session
}

type wakeRound struct {
	claims []WakeClaim
	done   chan struct{}
	winner *Session
}

// NewWakeArbiter 创建仲裁；window 是等其他终端的时间（v4 C-2：0.3 秒）。
func NewWakeArbiter(window time.Duration) *WakeArbiter {
	return &WakeArbiter{window: window, members: map[*Session]struct{}{}}
}

func (a *WakeArbiter) join(session *Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.members[session] = struct{}{}
}

func (a *WakeArbiter) leave(session *Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.members, session)
}

// claim 报一次唤醒，等这一轮结束，返回这台是不是被选中的那台。
func (a *WakeArbiter) claim(claim WakeClaim) bool {
	a.mu.Lock()
	if len(a.members) <= 1 && a.round == nil { // 只有一台在线：不用等
		a.mu.Unlock()
		return true
	}
	round := a.round
	if round == nil {
		round = &wakeRound{done: make(chan struct{})}
		a.round = round
		time.AfterFunc(a.window, func() { a.finish(round) })
	}
	round.claims = append(round.claims, claim)
	a.mu.Unlock()
	<-round.done
	return round.winner == claim.terminal
}

// finish 结束一轮：选出质量最高的那台。
func (a *WakeArbiter) finish(round *wakeRound) {
	a.mu.Lock()
	defer a.mu.Unlock()
	best := round.claims[0]
	for _, claim := range round.claims[1:] {
		if better(claim, best) {
			best = claim
		}
	}
	round.winner = best.terminal
	a.round = nil
	close(round.done)
}

// better 判断 x 的唤醒质量是否高于 y。
func better(x, y WakeClaim) bool {
	if x.Barge != y.Barge {
		return x.Barge
	}
	if diff := x.Score - y.Score; diff > scoreTie || diff < -scoreTie {
		return diff > 0
	}
	return x.Level > y.Level
}
