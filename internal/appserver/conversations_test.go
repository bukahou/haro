package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/chat"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/speech"
)

// echoModel 回一句固定的话。
type echoModel struct{}

func (echoModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, token := range []string{"好的，", "**记下了**。"} {
			if !yield(brain.ReplyDelta{Text: token}, nil) {
				return
			}
		}
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 10, OutputTokens: 2}, Stop: brain.StopCompleted}, nil)
	}
}

// titleModel 起一个固定的标题（比上限长：要截掉）。
type titleModel struct{}

func (titleModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		yield(brain.ReplyDelta{Text: "记事情的对话标题写得太长了要截掉一部分才行", Stop: brain.StopCompleted}, nil)
	}
}

func newChatFixture(t *testing.T) fixture {
	t.Helper()
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, err := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: brain.SharedPerson})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.IssueTokens(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := conversationstore.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conversations.Close() })
	options := brain.Options{MaxHistoryTurn: 6}
	book := brain.NewConversationBook(conversations, options, quietLogger)
	service := chat.NewService(ctx, conversations, func(speaker chat.Speaker, thread string) chat.TextSession {
		return brain.NewSession(brain.Components{Front: brain.Agent{Name: "前台", Model: echoModel{}, MaxSteps: 8, RepeatLimit: 3, RepeatWindow: 4},
			Conversations: book, Thread: thread, TextOnly: true,
			NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}, options, quietLogger)
	}, chat.AgentTitler{Agent: brain.Agent{Name: "起标题", Model: titleModel{}}}, quietLogger)
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY", MinAppVersion: "1.0.0",
		ProfileMaxChars: 10000, Chat: service, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return fixture{server: server, store: store, admin: admin, adminKey: pair}
}

// stream 发一句、读完事件流，返回各事件。
func (f fixture) stream(t *testing.T, conversation, token, body string) (int, []map[string]any) {
	t.Helper()
	request, _ := http.NewRequest("POST", f.server.URL+"/api/v1/conversations/"+conversation+"/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var events []map[string]any
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		event := map[string]any{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("事件解不开：%q", data)
		}
		events = append(events, event)
	}
	return response.StatusCode, events
}

// 打字：新建对话 → 说一句，回答以事件流回来（started → text… → done）→ 列表里有它（语音那条在最前）、翻得到这一轮；
// 语音那条也能接着打字；没登录 401、空的 400、语音那条不能删、停一个不存在的轮 404。
func TestChatOverHTTP(t *testing.T) {
	f := newChatFixture(t)
	key := f.adminKey.AccessToken
	status, created := f.call(t, "POST", "/api/v1/conversations", key, `{}`)
	if status != 201 || created["kind"] != "chat" {
		t.Fatalf("新建：%d %v", status, created)
	}
	id := created["conversation_id"].(string)
	status, events := f.stream(t, id, key, `{"text":"帮我记一下"}`)
	if status != 200 || len(events) < 3 || events[0]["type"] != "started" || events[0]["turn_id"] == "" ||
		events[len(events)-1]["type"] != "done" || events[len(events)-1]["status"] != "completed" {
		t.Fatalf("事件流：%d %v", status, events)
	}
	text := ""
	for _, event := range events {
		if event["type"] == "text" {
			text += event["text"].(string)
		}
	}
	if text != "好的，**记下了**。" {
		t.Errorf("回答的文字：%q", text)
	}
	conversations := f.list(t, "/api/v1/conversations", "zh")
	if len(conversations) != 2 || conversations[0]["conversation_id"] != "voice" || conversations[1]["last_text"] != "好的，**记下了**。" {
		t.Errorf("列表：%v", conversations)
	}
	status, page := f.call(t, "GET", "/api/v1/conversations/"+id+"/turns", key, "")
	turns, _ := page["turns"].([]any)
	if status != 200 || len(turns) != 1 || turns[0].(map[string]any)["said"] != "帮我记一下" || page["next_before"] != nil {
		t.Errorf("翻记录：%d %v", status, page)
	}
	waitTitle := time.Now().Add(2 * time.Second)
	for {
		listed := f.list(t, "/api/v1/conversations", "zh")
		if title, _ := listed[1]["title"].(string); title != "" {
			if utf8.RuneCountInString(title) != chat.TitleMaxRunes {
				t.Errorf("标题截到 %d 个字：%q", chat.TitleMaxRunes, title)
			}
			break
		}
		if time.Now().After(waitTitle) {
			t.Fatal("第一轮答完后应在后台起好标题")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, events := f.stream(t, "voice", key, `{"text":"语音那条也能打字"}`); status != 200 || events[len(events)-1]["type"] != "done" {
		t.Errorf("语音那条：%d %v", status, events)
	}
	if status, _ := f.stream(t, id, "", `{"text":"x"}`); status != 401 {
		t.Errorf("没登录：%d", status)
	}
	if status, _ := f.stream(t, id, key, `{"text":"  "}`); status != 400 {
		t.Errorf("空的：%d", status)
	}
	if status, _ := f.stream(t, "nope", key, `{"text":"x"}`); status != 404 {
		t.Errorf("没有这条对话：%d", status)
	}
	if status, body := f.call(t, "DELETE", "/api/v1/conversations/voice", key, ""); status != 409 || body["code"] != "conflict" {
		t.Errorf("语音那条不能删：%d %v", status, body)
	}
	if status, _ := f.call(t, "POST", "/api/v1/conversations/"+id+"/turns/x/stop", key, ""); status != 404 {
		t.Errorf("停一个不存在的轮：%d", status)
	}
	if maxMessageRunes != 4000 {
		t.Error("和契约 MessageSend.text 的 maxLength 对不上")
	}
}
