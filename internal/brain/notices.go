package brain

import (
	"sync"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// NoticeHub 把服务端给某个人的提示（例如后台调查查完了）和服务端要说的话（到点的提醒）送到他最近说话的那台终端。
// 所有终端连接共用一个；会话建立时加入、关闭时离开。
type NoticeHub struct {
	mu       sync.Mutex
	sessions map[*Session]*hubTerminal
	joined   func() // 有终端连上时调用（例如让提醒的调度看看有没有等着终端的提醒）；可以为 nil
}

// hubTerminal 是一台连着的终端：什么时候连上的、每个人最近一次在这台说话的时间。
type hubTerminal struct {
	joinedAt time.Time
	spoke    map[string]time.Time
}

// NewNoticeHub 创建提示中转。
func NewNoticeHub() *NoticeHub {
	return &NoticeHub{sessions: map[*Session]*hubTerminal{}}
}

// OnTerminalJoined 登记「有终端连上了」时要做的事（在连上的那个 goroutine 里调用，不能阻塞）。只登记一个，组装时调用。
func (h *NoticeHub) OnTerminalJoined(joined func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.joined = joined
}

func (h *NoticeHub) join(s *Session) {
	h.mu.Lock()
	h.sessions[s] = &hubTerminal{joinedAt: time.Now(), spoke: map[string]time.Time{}}
	joined := h.joined
	h.mu.Unlock()
	if joined != nil {
		joined()
	}
}

func (h *NoticeHub) leave(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, s)
}

// spoke 记下这个人刚在这台终端说过话。
func (h *NoticeHub) spoke(s *Session, person string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if terminal, ok := h.sessions[s]; ok {
		terminal.spoke[person] = time.Now()
	}
}

// latestFor 是 person 最近说话的那台终端；他在哪台都没说过话时为 nil。另外交出最近连上的那台（没有终端时为 nil）。
func (h *NoticeHub) latestFor(person string) (spokeLast, joinedLast *Session, all []*Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var spokeAt, joinedAt time.Time
	for s, terminal := range h.sessions {
		all = append(all, s)
		if at, ok := terminal.spoke[person]; ok && at.After(spokeAt) {
			spokeLast, spokeAt = s, at
		}
		if terminal.joinedAt.After(joinedAt) {
			joinedLast, joinedAt = s, terminal.joinedAt
		}
	}
	return spokeLast, joinedLast, all
}

// Notify 把提示送到 person 最近说话的那台终端；他在哪台都没说过话（例如那台终端断线重连过）就送到所有终端。
// 返回送到了几台。不阻塞：终端那边积压太多事件时丢掉这次提示（下次叫醒时任务板照样会说）。
func (h *NoticeHub) Notify(person string, notice realtimeproto.HaroNotice) int {
	latest, _, targets := h.latestFor(person)
	if latest != nil {
		targets = []*Session{latest}
	}
	delivered := 0
	for _, s := range targets {
		if s.tryEmit(notice) {
			delivered++
		}
	}
	return delivered
}

// Announce 让 announcement.Person 最近说话的那台终端说出这句话（v9 D5）；他在哪台都没说过话（例如服务端刚重启）时
// 交给最近连上的那一台（只说一遍：几台一起说，同一句会在几个房间响、历史里也记几遍）。
// 交出去了返回 true：之后恰好调用一次 announcement.Done。没有在线的终端、或那台排队满了返回 false（不调用 Done）。
func (h *NoticeHub) Announce(announcement Announcement) bool {
	latest, joinedLast, _ := h.latestFor(announcement.Person)
	target := latest
	if target == nil {
		target = joinedLast
	}
	return target != nil && target.announce(announcement)
}
