// Package reminders 是提醒：主人定好「十分钟后叫我」「每天早上八点提醒我吃药」，到点由 haro 主动说出来
// （v9 D1–D7，2026-11-22 用户裁定）。
//
//   - 时间由模型给（多少秒以后，或者带时区的 RFC 3339 时间），重复规则用 RFC 5545 的 RRULE；代码只校验和推算，
//     不理解「明早」「每周一」这样的话（P1）。时间一律存 UTC，按服务端的时区（TZ）推算钟点、给模型看。
//   - 到点时把定提醒时存下的那句话直接交给「开口」的一方（Announcer，服务端的会话）：不经模型，云端连不上照样说。
//   - 送不出去（这个人没有在线的终端）时，到点后 CatchUpWindow 以内终端连上就补说；超过就记成错过，
//     下次他开口时附在情况说明里告诉他。
//   - 存储（Store）和调度（Scheduler）分开，各在接口后面。
package reminders

import (
	"errors"
	"time"
)

// Reminder 是一条提醒。
type Reminder struct {
	ID          int64
	Person      string    // 定的人（声纹认出的名字）
	Text        string    // 到点要说的话
	Rule        string    // 重复规则（RRULE）；空 = 一次性
	StartsAt    time.Time // 第一次到点（UTC）
	NextAt      time.Time // 下一次到点（UTC）；零值 = 不会再到点
	CreatedAt   time.Time
	CancelledAt time.Time // 零值 = 在用
	SpokenAt    time.Time // 最近一次说出来的时间；零值 = 还没说过
	MissedAt    time.Time // 错过了、还没告诉主人的那一次到点；零值 = 没有
}

// Repeats 表示这是重复的提醒。
func (r Reminder) Repeats() bool { return r.Rule != "" }

// Active 表示在用：没取消、还会到点。
func (r Reminder) Active() bool { return r.CancelledAt.IsZero() && !r.NextAt.IsZero() }

// ErrNotFound：这个人没有这个编号的提醒（或者状态不对：要取消的已经取消了、要恢复的没有取消）。
var ErrNotFound = errors.New("没有这个编号的提醒")

// 上限与时长（有名字的常量，写明来历；都不开放成配置）。
const (
	// CatchUpWindow：到点时说不出来（服务端重启、终端断线）的提醒，这么久以内补说，更晚的只告诉主人错过了。
	// 来历：用户 2026-11-22 裁定 D6（例子即 30 分钟）
	CatchUpWindow = 30 * time.Minute
	// MaxActivePerPerson：每人在用的提醒最多这么多条（表要有上限，G6）。家里一个人远用不到；定值
	MaxActivePerPerson = 50
	// MaxAhead：一次性的提醒最远定到这么久以后。再远的事交给日历；定值
	MaxAhead = 366 * 24 * time.Hour
	// MaxTextRunes：到点要说的话最长多少字：一句提醒，念出来几秒；定值
	MaxTextRunes = 200
	// cancelledListLimit：列出最近取消的提醒时最多几条（找回用）
	cancelledListLimit = 20
)
