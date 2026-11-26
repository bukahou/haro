package reminders

import (
	"context"
	"time"
)

// Store 是提醒的存储。时间进出都是 UTC。按人的操作（Cancel、Restore 等）只动这个人的提醒。
type Store interface {
	// Add 存一条新的提醒，返回带编号和创建时间的那条。
	Add(ctx context.Context, reminder Reminder) (Reminder, error)
	// CountActive 是这个人在用的提醒有几条。
	CountActive(ctx context.Context, person string) (int, error)
	// Active 是这个人在用的提醒，按下一次到点排。
	Active(ctx context.Context, person string) ([]Reminder, error)
	// Cancelled 是这个人最近取消的 limit 条（最近取消的在前）。
	Cancelled(ctx context.Context, person string, limit int) ([]Reminder, error)
	// Get 取这个人的一条提醒（取消了的也算）；没有返回 ErrNotFound。
	Get(ctx context.Context, person string, id int64) (Reminder, error)
	// Cancel 取消这个人在用的一条（软删除）；没有或已经取消了返回 ErrNotFound。
	Cancel(ctx context.Context, person string, id int64, at time.Time) (Reminder, error)
	// Restore 恢复这个人取消了的一条，下一次到点改成 next；没有或没取消返回 ErrNotFound。
	Restore(ctx context.Context, person string, id int64, next time.Time) (Reminder, error)
	// Due 是所有人在用、到了 at（含）还没说的提醒，按到点排。
	Due(ctx context.Context, at time.Time) ([]Reminder, error)
	// NextDue 是所有在用的提醒里最早的下一次到点；没有在用的返回 false。
	NextDue(ctx context.Context) (time.Time, bool, error)
	// Spoken 记下这次到点说出来了：说的时间，下一次到点（零值 = 不会再到点）。已取消的不改。
	Spoken(ctx context.Context, id int64, at, next time.Time) error
	// Missed 记下这次到点错过了（missedAt 是那一次到点），下一次到点改成 next（零值 = 不会再到点）。已取消的不改。
	Missed(ctx context.Context, id int64, missedAt, next time.Time) error
	// Unannounced 是这个人错过了、还没告诉他的提醒（没取消的）。
	Unannounced(ctx context.Context, person string) ([]Reminder, error)
	// MissTold 记下错过的这几条已经告诉主人了。
	MissTold(ctx context.Context, ids ...int64) error
	Close() error
}
