package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/reminders"
)

// ReminderDesk 是提醒的工具要用的（reminders.Scheduler 实现）。
type ReminderDesk interface {
	Set(ctx context.Context, person string, request reminders.Request) (reminders.Reminder, error)
	List(ctx context.Context, person string, withCancelled bool) (reminders.Listing, error)
	Cancel(ctx context.Context, person string, id int64) (reminders.Reminder, error)
	Restore(ctx context.Context, person string, id int64) (reminders.Reminder, error)
	Location() *time.Location
	Now() time.Time
}

// 提醒的工具名（只在这里定义一处）。
const (
	SetReminderTool     = "set_reminder"
	ListRemindersTool   = "list_reminders"
	CancelReminderTool  = "cancel_reminder"
	RestoreReminderTool = "restore_reminder"
)

// errNotVerified：这句话的声纹没认出是登记过的人（v9 D4 c：只有本人的声音能定、恢复提醒；取消走确认闸，闸自己会查）。
var errNotVerified = errors.New("这句话没认出是登记过的人说的：只有本人的声音能定提醒、恢复提醒。照实告诉主人，请他再说一次")

// ReminderTools 是提醒的四个工具：定、查、取消（会破坏，过确认闸）、恢复。时间由模型算好交来（多少秒以后，或带时区的
// RFC 3339），重复规则用 RFC 5545 的 RRULE；代码只校验和推算，不理解「明早」「每周一」这样的话。
// 提醒按人记：只能查、取消、恢复自己的。
func ReminderTools(desk ReminderDesk, logger *slog.Logger) Builtin {
	logger = logger.With("tool", "reminders")
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: SetReminderTool, Annotations: Changes,
			Description: "定一个提醒：到点由你主动说出来（睡着时也会说）。主人说「十分钟后叫我」「明早七点提醒我吃药」「每周一提醒我倒垃圾」时用。" +
				"text 写到点时要直接对主人说的完整一句话，用主人说话的语言（例如「该吃药了」）。" +
				"时间二选一：after_seconds（多少秒以后，「十分钟后」这种用它）；at（RFC 3339，带时区偏移，例如 2026-11-27T07:00:00+09:00，" +
				"「明早七点」这种用它，按情况说明里现在的日期和时间算）。每天、每周这种重复的再加 rrule（RFC 5545，例如 FREQ=DAILY、" +
				"FREQ=WEEKLY;BYDAY=MO），第一次到点的时间用 at 给。定好后简短告诉主人什么时候提醒；这不是记事，不用说「记下了」。"},
			func(ctx context.Context, in setReminderInput) (reminderOutput, error) {
				if !brain.VoiceVerifiedFrom(ctx) {
					return reminderOutput{}, errNotVerified
				}
				request := reminders.Request{Text: in.Text, Rule: in.RRule}
				if in.AfterSeconds != nil {
					request.After = time.Duration(*in.AfterSeconds) * time.Second
				}
				if in.At != "" {
					at, err := time.Parse(time.RFC3339, in.At)
					if err != nil {
						return reminderOutput{}, brain.InvalidArguments(errors.New("at 要是 RFC 3339 的时间、带时区偏移（例如 2026-11-27T07:00:00+09:00）"))
					}
					request.At = at
				}
				reminder, err := desk.Set(ctx, brain.SpeakerFrom(ctx), request)
				logger.Info("定提醒", "id", reminder.ID, "err", err)
				if err != nil {
					return reminderOutput{}, err
				}
				return reminderOutput{Reminder: reminderView(reminder, desk)}, nil
			})
		AddTool(server, &mcp.Tool{Name: ListRemindersTool, Annotations: ReadOnly,
			Description: "列出主人在用的提醒（带编号，按下一次到点排；in_seconds 是离下一次到点还有多少秒）。" +
				"主人问有哪些提醒、还有多久、要取消或改某个提醒时，先用它找到编号。主人要找回取消了的提醒时 include_cancelled 填 true。"},
			func(ctx context.Context, in listRemindersInput) (reminderListOutput, error) {
				listing, err := desk.List(ctx, brain.SpeakerFrom(ctx), in.IncludeCancelled)
				if err != nil {
					return reminderListOutput{}, err
				}
				out := reminderListOutput{Reminders: []reminderJSON{}}
				for _, reminder := range listing.Active {
					out.Reminders = append(out.Reminders, reminderView(reminder, desk))
				}
				if in.IncludeCancelled {
					out.Cancelled = []reminderJSON{}
					for _, reminder := range listing.Cancelled {
						out.Cancelled = append(out.Cancelled, reminderView(reminder, desk))
					}
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: CancelReminderTool, Annotations: Destructive,
			Description: "按编号取消提醒（取消后可以恢复），一次可以取消多个。先用 " + ListRemindersTool + " 找到编号。" +
				"结果里 not_found 是没有的编号，要如实告诉主人。要改时间的，取消旧的再定一个新的。"},
			func(ctx context.Context, in cancelRemindersInput) (cancelRemindersOutput, error) {
				if len(in.IDs) == 0 {
					return cancelRemindersOutput{}, brain.InvalidArguments(errors.New("ids 不能为空：要说清楚取消哪几个"))
				}
				out := cancelRemindersOutput{Cancelled: []reminderJSON{}, NotFound: []int64{}}
				for _, id := range in.IDs {
					reminder, err := desk.Cancel(ctx, brain.SpeakerFrom(ctx), id)
					logger.Info("取消提醒", "id", id, "err", err)
					switch {
					case errors.Is(err, reminders.ErrNotFound):
						out.NotFound = append(out.NotFound, id)
					case err != nil:
						return out, err
					default:
						out.Cancelled = append(out.Cancelled, reminderView(reminder, desk))
					}
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: RestoreReminderTool, Annotations: Changes,
			Description: "按编号恢复一个取消了的提醒。先用 " + ListRemindersTool + "（include_cancelled 填 true）找到编号。" +
				"一次性的提醒时间已经过了就恢复不了，告诉主人要重新定。"},
			func(ctx context.Context, in restoreReminderInput) (reminderOutput, error) {
				if !brain.VoiceVerifiedFrom(ctx) {
					return reminderOutput{}, errNotVerified
				}
				reminder, err := desk.Restore(ctx, brain.SpeakerFrom(ctx), in.ID)
				logger.Info("恢复提醒", "id", in.ID, "err", err)
				if errors.Is(err, reminders.ErrNotFound) {
					return reminderOutput{}, brain.InvalidArguments(errors.New("没有这个编号的取消了的提醒"))
				}
				if err != nil {
					return reminderOutput{}, err
				}
				return reminderOutput{Reminder: reminderView(reminder, desk)}, nil
			})
	}
}

type setReminderInput struct {
	Text         string `json:"text" jsonschema:"到点时要直接对主人说的完整一句话（主人说话的语言）"`
	AfterSeconds *int64 `json:"after_seconds,omitempty" jsonschema:"多少秒以后提醒（和 at 二选一）"`
	At           string `json:"at,omitempty" jsonschema:"什么时候提醒：RFC 3339，带时区偏移（和 after_seconds 二选一）"`
	RRule        string `json:"rrule,omitempty" jsonschema:"重复规则（RFC 5545 RRULE，例如 FREQ=DAILY）；不重复就不填"`
}

type listRemindersInput struct {
	IncludeCancelled bool `json:"include_cancelled,omitempty" jsonschema:"同时列出最近取消的（找回用）"`
}

type cancelRemindersInput struct {
	IDs []int64 `json:"ids" jsonschema:"要取消的提醒的编号（list_reminders 结果里的 id），一次可以多个"`
}

type restoreReminderInput struct {
	ID int64 `json:"id" jsonschema:"要恢复的提醒的编号（list_reminders 带上 include_cancelled 时结果里的 id）"`
}

// reminderJSON 是交给模型的一条提醒（时间按服务端的时区写）。
type reminderJSON struct {
	ID          int64  `json:"id"`
	Text        string `json:"text"`
	NextAt      string `json:"next_at,omitempty"`    // 下一次到点；不会再到点时不写
	InSeconds   *int64 `json:"in_seconds,omitempty"` // 离下一次到点还有多少秒（在用的才有）
	RRule       string `json:"rrule,omitempty"`
	CancelledAt string `json:"cancelled_at,omitempty"`
}

type reminderOutput struct {
	Reminder reminderJSON `json:"reminder"`
}

type reminderListOutput struct {
	Reminders []reminderJSON `json:"reminders"`
	Cancelled []reminderJSON `json:"cancelled,omitempty"`
}

type cancelRemindersOutput struct {
	Cancelled []reminderJSON `json:"cancelled"`
	NotFound  []int64        `json:"not_found"`
}

func reminderView(reminder reminders.Reminder, desk ReminderDesk) reminderJSON {
	location := desk.Location()
	view := reminderJSON{ID: reminder.ID, Text: reminder.Text, RRule: reminder.Rule}
	if !reminder.NextAt.IsZero() {
		view.NextAt = reminder.NextAt.In(location).Format(time.RFC3339)
		if reminder.Active() {
			in := int64(reminder.NextAt.Sub(desk.Now()).Seconds())
			view.InSeconds = &in
		}
	}
	if !reminder.CancelledAt.IsZero() {
		view.CancelledAt = reminder.CancelledAt.In(location).Format(time.RFC3339)
	}
	return view
}
