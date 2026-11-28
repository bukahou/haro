package appserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/calendar"
)

// 日历的业务参数（client-config 给 App；定值）。
const (
	calendarSyncDaysBack  = 30
	calendarSyncDaysAhead = 90
	// maxOccurrenceSpan 是一次最多查多长一段（和契约 listOccurrences 的说明一致）。
	maxOccurrenceSpan = 92 * 24 * time.Hour
	// maxAlertsAhead 是手机的提醒清单最多往后多久。
	maxAlertsAhead = 30 * 24 * time.Hour
	// syncPattern 是同步系统日历的路由（请求体比别的接口大：一个日历几个月的日程）。
	syncPattern = "PUT " + PathPrefix + "calendar/syncs/{calendarId}"
	// syncMaxBytes 是同步一次的请求体上限。
	syncMaxBytes = 2 << 20
)

// Calendar 是日历（calendar.Service 实现）。
type Calendar interface {
	Create(ctx context.Context, person string, input calendar.Input) (calendar.Event, error)
	Get(ctx context.Context, person, id string) (calendar.Event, error)
	Update(ctx context.Context, person, id string, input calendar.Input, scope calendar.Scope) (calendar.Event, error)
	Delete(ctx context.Context, person, id string, scope calendar.Scope) error
	Occurrences(ctx context.Context, person string, from, to time.Time) ([]calendar.Occurrence, error)
	PhoneAlerts(ctx context.Context, person string, until time.Time) ([]calendar.PhoneAlert, error)
	Sync(ctx context.Context, person, calendarID, name string, events []calendar.SyncedEvent) (calendar.SyncResult, error)
	Unsync(ctx context.Context, person, calendarID string) error
	Syncs(ctx context.Context, person string) ([]calendar.Sync, error)
}

func eventOf(event calendar.Event) appapi.CalendarEvent {
	alerts := make([]int, 0, len(event.Alerts))
	for _, lead := range event.Alerts {
		alerts = append(alerts, int(lead.Seconds()))
	}
	return appapi.CalendarEvent{EventId: event.ID, Title: event.Title, Location: event.Location, Notes: event.Notes, Start: event.Start,
		End: event.End, AllDay: event.AllDay, Rrule: optional(event.Rule), Alerts: alerts, AlertTerminal: event.AlertTerminal,
		AlertPhone: event.AlertPhone, Source: appapi.CalendarSource(event.Source), CalendarName: optional(event.CalendarName),
		Editable: event.Editable()}
}

func inputOf(ctx context.Context, body appapi.EventInput) calendar.Input {
	input := calendar.Input{Title: body.Title, Location: deref(body.Location), Notes: deref(body.Notes), Start: body.Start, End: body.End,
		AllDay: body.AllDay, Rule: deref(body.Rrule), AlertTerminal: body.AlertTerminal, AlertPhone: body.AlertPhone, Language: languageFrom(ctx)}
	for _, lead := range body.Alerts {
		input.Alerts = append(input.Alerts, time.Duration(lead)*time.Second)
	}
	return input
}

// calendarProblem 是日历的错误对应的回答（按错误的类型分）；认不出的是 500。
func (s *Server) calendarProblem(ctx context.Context, what string, err error) problemReply {
	switch {
	case errors.Is(err, calendar.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
	case errors.Is(err, calendar.ErrReadOnly):
		return s.fail(ctx, http.StatusConflict, appapi.ErrorCodeConflict, nil)
	case errors.Is(err, calendar.ErrInvalid):
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": err.Error()})
	}
	return s.internalError(ctx, what, err)
}

// ListOccurrences 实现 appapi.StrictServerInterface。
func (s *Server) ListOccurrences(ctx context.Context, request appapi.ListOccurrencesRequestObject) (appapi.ListOccurrencesResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	from, to := request.Params.From, request.Params.To
	if !to.After(from) || to.Sub(from) > maxOccurrenceSpan {
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "from / to"}), nil
	}
	occurrences, err := s.opts.Calendar.Occurrences(ctx, account.OwnerPerson, from, to)
	if err != nil {
		return s.calendarProblem(ctx, "读日程", err), nil
	}
	out := appapi.ListOccurrences200JSONResponse{}
	for _, occurrence := range occurrences {
		out = append(out, appapi.Occurrence{Event: eventOf(occurrence.Event), Start: occurrence.Start, End: occurrence.End,
			Recurring: occurrence.Event.Rule != ""})
	}
	return out, nil
}

// CreateEvent 实现 appapi.StrictServerInterface。
func (s *Server) CreateEvent(ctx context.Context, request appapi.CreateEventRequestObject) (appapi.CreateEventResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	event, err := s.opts.Calendar.Create(ctx, account.OwnerPerson, inputOf(ctx, *request.Body))
	if err != nil {
		return s.calendarProblem(ctx, "建日程", err), nil
	}
	return appapi.CreateEvent201JSONResponse(eventOf(event)), nil
}

// GetEvent 实现 appapi.StrictServerInterface。
func (s *Server) GetEvent(ctx context.Context, request appapi.GetEventRequestObject) (appapi.GetEventResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	event, err := s.opts.Calendar.Get(ctx, account.OwnerPerson, request.EventId)
	if err != nil {
		return s.calendarProblem(ctx, "读日程", err), nil
	}
	return appapi.GetEvent200JSONResponse(eventOf(event)), nil
}

// scopeOf 是改、删管到哪。
func scopeOf(kind *string, occurrence *time.Time) calendar.Scope {
	scope := calendar.Scope{Kind: deref(kind)}
	if occurrence != nil {
		scope.At = *occurrence
	}
	return scope
}

// UpdateEvent 实现 appapi.StrictServerInterface。
func (s *Server) UpdateEvent(ctx context.Context, request appapi.UpdateEventRequestObject) (appapi.UpdateEventResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	var kind *string
	if request.Params.Scope != nil {
		value := string(*request.Params.Scope)
		kind = &value
	}
	event, err := s.opts.Calendar.Update(ctx, account.OwnerPerson, request.EventId, inputOf(ctx, *request.Body),
		scopeOf(kind, request.Params.Occurrence))
	if err != nil {
		return s.calendarProblem(ctx, "改日程", err), nil
	}
	return appapi.UpdateEvent200JSONResponse(eventOf(event)), nil
}

// DeleteEvent 实现 appapi.StrictServerInterface。
func (s *Server) DeleteEvent(ctx context.Context, request appapi.DeleteEventRequestObject) (appapi.DeleteEventResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	var kind *string
	if request.Params.Scope != nil {
		value := string(*request.Params.Scope)
		kind = &value
	}
	if err := s.opts.Calendar.Delete(ctx, account.OwnerPerson, request.EventId, scopeOf(kind, request.Params.Occurrence)); err != nil {
		return s.calendarProblem(ctx, "删日程", err), nil
	}
	return appapi.DeleteEvent204Response{}, nil
}

// ListPhoneAlerts 实现 appapi.StrictServerInterface。
func (s *Server) ListPhoneAlerts(ctx context.Context, request appapi.ListPhoneAlertsRequestObject) (appapi.ListPhoneAlertsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	if time.Until(request.Params.Until) > maxAlertsAhead {
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "until"}), nil
	}
	alerts, err := s.opts.Calendar.PhoneAlerts(ctx, account.OwnerPerson, request.Params.Until)
	if err != nil {
		return s.calendarProblem(ctx, "读提醒", err), nil
	}
	out := appapi.ListPhoneAlerts200JSONResponse{}
	for _, alert := range alerts {
		out = append(out, appapi.PhoneAlert{EventId: alert.Occurrence.Event.ID, Title: alert.Occurrence.Event.Title,
			OccurrenceStart: alert.Occurrence.Start, At: alert.At, LeadSeconds: int(alert.Lead.Seconds())})
	}
	return out, nil
}

// ListCalendarSyncs 实现 appapi.StrictServerInterface。
func (s *Server) ListCalendarSyncs(ctx context.Context, _ appapi.ListCalendarSyncsRequestObject) (appapi.ListCalendarSyncsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	syncs, err := s.opts.Calendar.Syncs(ctx, account.OwnerPerson)
	if err != nil {
		return s.calendarProblem(ctx, "读同步", err), nil
	}
	out := appapi.ListCalendarSyncs200JSONResponse{}
	for _, sync := range syncs {
		out = append(out, appapi.CalendarSync{CalendarId: sync.Calendar, Name: sync.Name, SyncedAt: sync.SyncedAt})
	}
	return out, nil
}

// SyncCalendar 实现 appapi.StrictServerInterface。
func (s *Server) SyncCalendar(ctx context.Context, request appapi.SyncCalendarRequestObject) (appapi.SyncCalendarResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	events := make([]calendar.SyncedEvent, 0, len(request.Body.Events))
	for _, event := range request.Body.Events {
		events = append(events, calendar.SyncedEvent{ExternalID: event.ExternalId, Title: event.Title, Location: deref(event.Location),
			Notes: deref(event.Notes), Start: event.Start, End: event.End, AllDay: event.AllDay})
	}
	result, err := s.opts.Calendar.Sync(ctx, account.OwnerPerson, request.CalendarId, request.Body.Name, events)
	if err != nil {
		return s.calendarProblem(ctx, "同步系统日历", err), nil
	}
	return appapi.SyncCalendar200JSONResponse{Added: result.Added, Updated: result.Updated, Removed: result.Removed}, nil
}

// UnsyncCalendar 实现 appapi.StrictServerInterface。
func (s *Server) UnsyncCalendar(ctx context.Context, request appapi.UnsyncCalendarRequestObject) (appapi.UnsyncCalendarResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Calendar == nil {
		return s.notImplemented(ctx), nil
	}
	if err := s.opts.Calendar.Unsync(ctx, account.OwnerPerson, request.CalendarId); err != nil {
		return s.calendarProblem(ctx, "不再同步系统日历", err), nil
	}
	return appapi.UnsyncCalendar204Response{}, nil
}
