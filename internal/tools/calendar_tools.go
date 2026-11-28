package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/calendar"
)

// ListCalendarTool 是查日程的工具名（只在这里定义一处）。
const ListCalendarTool = "list_calendar"

// maxCalendarSpan：查日程一次最多查多长一段（太长的结果模型也念不完）。定值
const maxCalendarSpan = 62 * 24 * time.Hour

// Calendar 是查日程用到的（calendar.Service 实现）。
type Calendar interface {
	Occurrences(ctx context.Context, person string, from, to time.Time) ([]calendar.Occurrence, error)
}

type listCalendarInput struct {
	From string `json:"from" jsonschema:"从什么时候起（RFC 3339，带时区偏移，例如 2026-11-29T00:00:00+09:00；按情况说明里现在的日期和时间算）"`
	To   string `json:"to" jsonschema:"到什么时候为止（不含；RFC 3339，带时区偏移）"`
}

type calendarEntry struct {
	Title    string `json:"title"`
	Start    string `json:"start"`         // 定时的：当地时间；全天的：日期
	End      string `json:"end,omitempty"` // 定时的、有结束时间的
	AllDay   bool   `json:"all_day,omitempty"`
	Location string `json:"location,omitempty"`
	Notes    string `json:"notes,omitempty"`
	Calendar string `json:"calendar,omitempty"` // 从哪个系统日历同步进来的；haro 自己的为空
}

type listCalendarOutput struct {
	Entries []calendarEntry `json:"entries"`
}

// CalendarTools 是查日程的工具：主人问「明天有什么安排」「这周末有空吗」时，按时间段列出日历里的每一次（haro 自己的和系统日历同步进来的）。
// 时间由模型按现在的日期算好交来；代码只校验和展开。
func CalendarTools(events Calendar, location *time.Location, logger *slog.Logger) Builtin {
	logger = logger.With("tool", "calendar")
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: ListCalendarTool, Annotations: ReadOnly,
			Description: "查主人的日程：列出一段时间里的每一个安排（haro 自己记的，和主人手机里系统日历同步进来的）。主人问「明天有什么安排」" +
				"「这周末有空吗」「下次看牙医是什么时候」时用。from、to 按情况说明里现在的日期和时间算。"},
			func(ctx context.Context, in listCalendarInput) (listCalendarOutput, error) {
				from, errFrom := time.Parse(time.RFC3339, in.From)
				to, errTo := time.Parse(time.RFC3339, in.To)
				switch {
				case errFrom != nil || errTo != nil:
					return listCalendarOutput{}, brain.InvalidArguments(errors.New("from、to 要是 RFC 3339 的时间、带时区偏移"))
				case !to.After(from):
					return listCalendarOutput{}, brain.InvalidArguments(errors.New("to 要晚于 from"))
				case to.Sub(from) > maxCalendarSpan:
					return listCalendarOutput{}, brain.InvalidArguments(errors.New("一次最多查两个月：分几段查"))
				}
				occurrences, err := events.Occurrences(ctx, brain.SpeakerFrom(ctx), from, to)
				logger.Info("查日程", "from", from, "to", to, "found", len(occurrences), "err", err)
				if err != nil {
					return listCalendarOutput{}, err
				}
				out := listCalendarOutput{Entries: []calendarEntry{}}
				for _, occurrence := range occurrences {
					event := occurrence.Event
					entry := calendarEntry{Title: event.Title, AllDay: event.AllDay, Location: event.Location, Notes: event.Notes,
						Calendar: event.CalendarName}
					if event.AllDay {
						entry.Start = occurrence.Start.Format(time.DateOnly)
					} else {
						entry.Start = occurrence.Start.In(location).Format(time.RFC3339)
						if occurrence.End.After(occurrence.Start) {
							entry.End = occurrence.End.In(location).Format(time.RFC3339)
						}
					}
					out.Entries = append(out.Entries, entry)
				}
				return out, nil
			})
	}
}
