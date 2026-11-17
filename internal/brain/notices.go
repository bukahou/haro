package brain

import (
	"sync"
	"time"

	"haro/internal/realtimeproto"
)

// NoticeHub 把大脑给某个人的提示（例如后台调查查完了）送到他最近说话的那台终端（v5：大脑还不能主动开口，
// 先让终端响一声）。所有终端连接共用一个；会话建立时加入、关闭时离开。
type NoticeHub struct {
	mu       sync.Mutex
	sessions map[*Session]map[string]time.Time // 会话 → 每个人最近一次在这台终端说话的时间
}

// NewNoticeHub 创建提示中转。
func NewNoticeHub() *NoticeHub {
	return &NoticeHub{sessions: map[*Session]map[string]time.Time{}}
}

func (h *NoticeHub) join(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[s] = map[string]time.Time{}
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
	if people, ok := h.sessions[s]; ok {
		people[person] = time.Now()
	}
}

// Notify 把提示送到 person 最近说话的那台终端；他在哪台都没说过话（例如那台终端断线重连过）就送到所有终端。
// 返回送到了几台。不阻塞：终端那边积压太多事件时丢掉这次提示（下次叫醒时任务板照样会说）。
func (h *NoticeHub) Notify(person string, notice realtimeproto.HaroNotice) int {
	h.mu.Lock()
	var latest *Session
	var latestAt time.Time
	targets := make([]*Session, 0, len(h.sessions))
	for s, people := range h.sessions {
		targets = append(targets, s)
		if at, ok := people[person]; ok && at.After(latestAt) {
			latest, latestAt = s, at
		}
	}
	h.mu.Unlock()
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
