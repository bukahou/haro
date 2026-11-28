package appserver

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/calendar"
)

// 日历：建（每周的、提前 10 分钟）→ 展开的每一次 → 只改其中一次 → 手机的提醒 → 系统日历同步（只读，改它 409）→ 删；不合规的 400。
func TestCalendarOverHTTP(t *testing.T) {
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, _ := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: brain.SharedPerson})
	pair, _ := store.IssueTokens(ctx, admin.ID)
	events, err := calendar.OpenStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	service := calendar.NewService(events, calendar.Options{Location: time.UTC, Logger: quietLogger})
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY", MinAppVersion: "1.0.0",
		ProfileMaxChars: 10000, Calendar: service, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := fixture{server: server, store: store, admin: admin, adminKey: pair}
	key := pair.AccessToken
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
	weekday := []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[start.Weekday()]
	body := fmt.Sprintf(`{"title":"英语课","start":%q,"end":%q,"all_day":false,"rrule":"FREQ=WEEKLY;BYDAY=%s","alerts":[600],
		"alert_terminal":true,"alert_phone":true}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339), weekday)
	status, created := f.call(t, "POST", "/api/v1/calendar/events", key, body)
	if status != 201 || created["editable"] != true || created["source"] != "haro" {
		t.Fatalf("建：%d %v", status, created)
	}
	id := created["event_id"].(string)
	window := "/api/v1/calendar/occurrences?from=" + url.QueryEscape(start.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(start.AddDate(0, 0, 21).Format(time.RFC3339))
	occurrences := f.list(t, window, "zh")
	if len(occurrences) != 3 || occurrences[0]["recurring"] != true {
		t.Fatalf("三周：%v", occurrences)
	}
	second := occurrences[1]["start"].(string)
	moved := fmt.Sprintf(`{"title":"英语课（改时间）","start":%q,"end":%q,"all_day":false,"alerts":[],"alert_terminal":false,"alert_phone":false}`,
		start.AddDate(0, 0, 8).Format(time.RFC3339), start.AddDate(0, 0, 8).Add(time.Hour).Format(time.RFC3339))
	if status, changed := f.call(t, "PATCH", "/api/v1/calendar/events/"+id+"?scope=this&occurrence="+url.QueryEscape(second), key, moved); status != 200 ||
		changed["title"] != "英语课（改时间）" || changed["rrule"] != nil {
		t.Fatalf("只改这一次：%d %v", status, changed)
	}
	occurrences = f.list(t, window, "zh")
	if len(occurrences) != 3 || occurrences[1]["event"].(map[string]any)["title"] != "英语课（改时间）" {
		t.Errorf("改后：%v", occurrences)
	}
	until := url.QueryEscape(time.Now().Add(15 * 24 * time.Hour).Format(time.RFC3339))
	if alerts := f.list(t, "/api/v1/calendar/alerts?until="+until, "zh"); len(alerts) != 2 || alerts[0]["lead_seconds"] != float64(600) {
		t.Errorf("手机的提醒（改了的那次不响）：%v", alerts)
	}
	sync := fmt.Sprintf(`{"name":"工作","events":[{"external_id":"x1","title":"周会","start":%q,"end":%q,"all_day":false}]}`,
		start.Add(2*time.Hour).Format(time.RFC3339), start.Add(3*time.Hour).Format(time.RFC3339))
	if status, result := f.call(t, "PUT", "/api/v1/calendar/syncs/work-1", key, sync); status != 200 || result["added"] != float64(1) {
		t.Fatalf("同步：%d %v", status, result)
	}
	var synced string
	for _, occurrence := range f.list(t, window, "zh") {
		if event := occurrence["event"].(map[string]any); event["source"] == "system" {
			synced = event["event_id"].(string)
		}
	}
	if status, _ := f.call(t, "PATCH", "/api/v1/calendar/events/"+synced, key, moved); status != 409 {
		t.Errorf("同步进来的只读：%d", status)
	}
	if syncs := f.list(t, "/api/v1/calendar/syncs", "zh"); len(syncs) != 1 || syncs[0]["name"] != "工作" {
		t.Errorf("上次同步：%v", syncs)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/calendar/syncs/work-1", key, ""); status != 204 {
		t.Errorf("不再同步：%d", status)
	}
	for _, occurrence := range f.list(t, window, "zh") {
		if occurrence["event"].(map[string]any)["source"] == "system" {
			t.Errorf("不再同步后它的日程应去掉：%v", occurrence)
		}
	}
	if syncs := f.list(t, "/api/v1/calendar/syncs", "zh"); len(syncs) != 0 {
		t.Errorf("同步记录应删掉：%v", syncs)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/calendar/syncs/work-1", key, ""); status != 404 {
		t.Errorf("没同步过的：%d", status)
	}
	bad := fmt.Sprintf(`{"title":"错","start":%q,"end":%q,"all_day":false,"rrule":"FREQ=NEVER","alerts":[],"alert_terminal":true,"alert_phone":true}`,
		start.Format(time.RFC3339), start.Format(time.RFC3339))
	if status, body := f.call(t, "POST", "/api/v1/calendar/events", key, bad); status != 400 || body["code"] != "validation_failed" {
		t.Errorf("不合规的规则：%d %v", status, body)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/calendar/events/"+id, key, ""); status != 204 {
		t.Errorf("删：%d", status)
	}
	if status, _ := f.call(t, "GET", "/api/v1/calendar/events/"+id, key, ""); status != 404 {
		t.Errorf("删了以后：%d", status)
	}
}
