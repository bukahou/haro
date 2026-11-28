package appserver

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/backstory"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/research"
)

// 契约里调查的状态和 research 的状态是同一组；每个状态都有给人看的文字（每种语言）。
func TestResearchStatusesMatchContract(t *testing.T) {
	spec, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var contract []string
	for _, value := range spec.Components.Schemas["ResearchStatus"].Value.Enum {
		contract = append(contract, value.(string))
	}
	code := []string{string(research.StatusQueued), string(research.StatusRunning), string(research.StatusDone), string(research.StatusPartial),
		string(research.StatusFailed), string(research.StatusCancelled), string(research.StatusInterrupted)}
	slices.Sort(contract)
	slices.Sort(code)
	if !slices.Equal(contract, code) {
		t.Errorf("契约 %v，代码 %v", contract, code)
	}
	loaded, err := loadTexts()
	if err != nil {
		t.Fatal(err)
	}
	for language, entries := range loaded.Languages {
		for _, status := range code {
			if entries["research."+status] == "" {
				t.Errorf("%s 缺少调查状态 %s 的文字", language, status)
			}
		}
	}
}

// fakeMemory 是两条记过的事。
type fakeMemory struct{ forgotten map[int64]bool }

func (f *fakeMemory) Counts(context.Context, string) (int, int, error) { return 1, 1, nil }
func (f *fakeMemory) List(context.Context, string, bool) (memory.Listing, error) {
	listing := memory.Listing{Total: 2}
	for _, note := range []notes.Note{{ID: 1, Content: "猫叫小橘", Resident: true}, {ID: 2, Content: "周三倒垃圾"}} {
		if f.forgotten[note.ID] {
			note.DeletedAt = time.Now()
			listing.Forgotten = append(listing.Forgotten, note)
		} else {
			listing.Notes = append(listing.Notes, note)
		}
	}
	return listing, nil
}
func (f *fakeMemory) Forget(_ context.Context, _ string, id int64) (notes.Note, error) {
	if id > 2 {
		return notes.Note{}, notes.ErrNotFound
	}
	f.forgotten[id] = true
	return notes.Note{ID: id}, nil
}
func (f *fakeMemory) Restore(_ context.Context, _ string, id int64) (notes.Note, error) {
	if !f.forgotten[id] {
		return notes.Note{}, notes.ErrNotFound
	}
	delete(f.forgotten, id)
	return notes.Note{ID: id, Content: "周三倒垃圾"}, nil
}

// 背景记录池：读（带出处）、改（之后算主人改过的）、删、关掉；记过的事：列出、忘掉、找回。
func TestBackstoryAndNotesOverHTTP(t *testing.T) {
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, _ := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: brain.SharedPerson})
	pair, _ := store.IssueTokens(ctx, admin.ID)
	conversations, err := conversationstore.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conversations.Close() })
	if err := conversations.ReplaceGenerated(ctx, "主人", []conversationstore.BackstoryItem{{ID: "b1", Text: "最近在学日语", SourceSeq: 3}},
		3, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	pool, err := backstory.Open(ctx, conversations, nil, time.Hour, quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY", MinAppVersion: "1.0.0",
		ProfileMaxChars: 10000, Backstory: pool, Memory: &fakeMemory{forgotten: map[int64]bool{}}, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := fixture{server: server, store: store, admin: admin, adminKey: pair}
	key := pair.AccessToken
	status, body := f.call(t, "GET", "/api/v1/memory/backstory", key, "")
	items, _ := body["items"].([]any)
	if status != 200 || body["enabled"] != true || len(items) != 1 || items[0].(map[string]any)["source"].(map[string]any)["conversation_id"] != "voice" {
		t.Fatalf("读：%d %v", status, body)
	}
	status, body = f.call(t, "PATCH", "/api/v1/memory/backstory/items/b1", key, `{"text":"在学日语，准备 N2"}`)
	items, _ = body["items"].([]any)
	if status != 200 || items[0].(map[string]any)["edited"] != true {
		t.Errorf("改：%d %v", status, body)
	}
	if status, _ := f.call(t, "PATCH", "/api/v1/memory/backstory/items/nope", key, `{"text":"x"}`); status != 404 {
		t.Errorf("没有这一条：%d", status)
	}
	if status, body := f.call(t, "PATCH", "/api/v1/memory/backstory", key, `{"enabled":false}`); status != 200 || body["enabled"] != false {
		t.Errorf("关掉：%d %v", status, body)
	}
	if pool.Block("主人") != "" {
		t.Error("关了就不带")
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/memory/backstory/items/b1", key, ""); status != 204 {
		t.Errorf("删：%d", status)
	}
	if status, body := f.call(t, "GET", "/api/v1/memory/notes", key, ""); status != 200 || len(body["notes"].([]any)) != 2 {
		t.Errorf("记过的事：%d %v", status, body)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/memory/notes/2", key, ""); status != 204 {
		t.Errorf("忘掉：%d", status)
	}
	if status, body := f.call(t, "GET", "/api/v1/memory/notes", key, ""); status != 200 || len(body["forgotten"].([]any)) != 1 {
		t.Errorf("忘掉的能看到：%d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/v1/memory/notes/2/restore", key, ""); status != 200 || body["content"] != "周三倒垃圾" {
		t.Errorf("找回：%d %v", status, body)
	}
	if status, _ := f.call(t, "POST", "/api/v1/memory/notes/9/restore", key, ""); status != 404 {
		t.Errorf("找回没有的：%d", status)
	}
}
