package brain

import (
	"io"
	"log/slog"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
)

func newHubSession(hub *NoticeHub) *Session {
	return NewSession(Components{Notices: hub}, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func received(s *Session) int {
	n := 0
	for {
		select {
		case event := <-s.Events():
			if _, ok := event.(realtimeproto.HaroNotice); ok {
				n++
			}
		default:
			return n
		}
	}
}

// 提示送到这个人最近说话的那台终端；他在哪台都没说过话就送到所有终端；关掉的终端不送。
func TestNoticeGoesToWhereThePersonLastSpoke(t *testing.T) {
	hub := NewNoticeHub()
	kitchen, bedroom := newHubSession(hub), newHubSession(hub)
	defer kitchen.Close()
	notice := realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone}

	if n := hub.Notify("主人", notice); n != 2 || received(kitchen) != 1 || received(bedroom) != 1 {
		t.Fatalf("没在任何终端说过话：应送到所有终端（%d）", n)
	}
	kitchen.useConversationOf("主人")
	bedroom.useConversationOf("主人") // 后在卧室说的
	if n := hub.Notify("主人", notice); n != 1 || received(bedroom) != 1 || received(kitchen) != 0 {
		t.Fatalf("应只送到最近说话的卧室（%d）", n)
	}
	bedroom.Close()
	if n := hub.Notify("主人", notice); n != 1 || received(kitchen) != 1 {
		t.Fatalf("卧室关了：送到还连着的厨房（%d）", n)
	}
}
