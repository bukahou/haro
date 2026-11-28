package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/chat"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/speech"
)

// echoModel 回一句固定的话（带一个情绪标记：那是给语音用的，流给 App 的和记录里都不该有）。
type echoModel struct{}

func (echoModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, token := range []string{"[温柔]好的，", "**记下了**。"} {
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
	return newChatFixtureWith(t, echoModel{}, nil)
}

// newChatFixtureWith 是打字对话的夹具：前台用 model、工具用 tools（nil = 没有工具）。
func newChatFixtureWith(t *testing.T, model brain.ChatModel, tools brain.Toolbox) fixture {
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
	images, err := attachments.OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := chat.NewService(ctx, chat.Options{Store: conversations, Images: images, Logger: quietLogger,
		Titler:  chat.AgentTitler{Agent: brain.Agent{Name: "起标题", Model: titleModel{}}},
		Actions: book,
		Sessions: func(speaker chat.Speaker, thread string, activity brain.ActivityObserver) chat.TextSession {
			parts := brain.Components{Front: brain.Agent{Name: "前台", Model: model, MaxSteps: 8, RepeatLimit: 3, RepeatWindow: 4},
				Conversations: book, Thread: thread, TextOnly: true, Activity: activity,
				NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}
			if tools != nil {
				parts.Tools = tools
			}
			return brain.NewSession(parts, options, quietLogger)
		}})
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
	return f.events(t, "/api/v1/conversations/"+conversation+"/messages", token, body)
}

// events 向 path 发请求、读完事件流，返回各事件。
func (f fixture) events(t *testing.T, path, token, body string) (int, []map[string]any) {
	t.Helper()
	request, _ := http.NewRequest("POST", f.server.URL+path, strings.NewReader(body))
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
		t.Fatalf("翻记录：%d %v", status, page)
	}
	if blocks := turns[0].(map[string]any)["reply"].([]any); len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "好的，**记下了**。" {
		t.Errorf("记录里的回答（不带情绪标记）：%v", blocks)
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

// upload 传一张图片。
func (f fixture) upload(t *testing.T, conversation, contentType string, data []byte) (int, map[string]any) {
	t.Helper()
	request, _ := http.NewRequest("POST", f.server.URL+"/api/v1/conversations/"+conversation+"/attachments", bytes.NewReader(data))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer "+f.adminKey.AccessToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, body
}

// 图片：在对话里传（PNG 存成 JPEG）→ 带着它说一句 → 记录里这一轮带附件编号 → 取回是 JPEG；格式不对、别的对话的图片不收。
// 搜以前的对话：按文字找得到（带在哪条对话）。
func TestAttachmentsAndSearch(t *testing.T) {
	f := newChatFixture(t)
	key := f.adminKey.AccessToken
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	status, info := f.upload(t, "voice", "image/png", encoded.Bytes())
	if status != 201 || info["width"] != float64(40) {
		t.Fatalf("上传：%d %v", status, info)
	}
	id := info["attachment_id"].(string)
	if status, _ := f.upload(t, "voice", "text/plain", []byte("x")); status != 400 {
		t.Errorf("格式不对：%d", status)
	}
	if status, _ := f.upload(t, "voice", "image/png", []byte("不是图片")); status != 400 {
		t.Errorf("解不开：%d", status)
	}
	status, events := f.stream(t, "voice", key, fmt.Sprintf(`{"text":"这是什么猫","attachment":%q}`, id))
	if status != 200 || events[len(events)-1]["type"] != "done" {
		t.Fatalf("带图说一句：%d %v", status, events)
	}
	status, page := f.call(t, "GET", "/api/v1/conversations/voice/turns", key, "")
	turns, _ := page["turns"].([]any)
	if status != 200 || len(turns) != 1 || turns[0].(map[string]any)["attachment"] != id {
		t.Errorf("记录里带附件：%d %v", status, page)
	}
	request, _ := http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+id, nil)
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, decodeErr := jpeg.DecodeConfig(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "image/jpeg" || decodeErr != nil {
		t.Errorf("取回：%d %s %v", response.StatusCode, response.Header.Get("Content-Type"), decodeErr)
	}
	_, created := f.call(t, "POST", "/api/v1/conversations", key, `{}`)
	if status, _ := f.stream(t, created["conversation_id"].(string), key, fmt.Sprintf(`{"text":"再看看","attachment":%q}`, id)); status != 400 {
		t.Errorf("别的对话的图片：%d", status)
	}
	found := f.list(t, "/api/v1/turns/search?q=%E7%8C%AB", "zh")
	if len(found) != 1 || found[0]["conversation_id"] != "voice" || found[0]["kind"] != "voice" || found[0]["title"] != nil {
		t.Errorf("搜「猫」（带上是哪种对话、标题）：%v", found)
	}
	contract, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for media := range contract.Paths.Find("/api/v1/conversations/{conversationId}/attachments").Post.RequestBody.Value.Content {
		types = append(types, media)
	}
	slices.Sort(types)
	if want := slices.Sorted(slices.Values(uploadedTypes)); !slices.Equal(types, want) {
		t.Errorf("契约里收的图片格式 %v，代码里 %v", types, want)
	}
}

// gateToolbox 有一个只读的搜索和一个会破坏的删除（要过确认闸），记下执行过的。
type gateToolbox struct {
	mu    sync.Mutex
	calls []string
}

func (g *gateToolbox) Specs() []brain.ToolSpec {
	return []brain.ToolSpec{{Name: "web_search", Parameters: json.RawMessage(`{}`)},
		{Name: "delete_note", Parameters: json.RawMessage(`{}`), RequiresConfirmation: true}}
}

func (g *gateToolbox) Call(_ context.Context, call brain.ToolCall) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call.Name)
	return `{"ok":true}`, nil
}

func (g *gateToolbox) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// 打字对话里的工具进度和确认卡片：事件流里有工具开始 / 做完（带给人看的说明）、确认卡片；记录里的工具带说明、卡片还能点；
// 点卡片上的确认（decideConfirmation）才执行，之后记录里那张卡片不能再点。请求体不对 400、没有这条对话 404。
func TestChatActivityAndConfirmationCard(t *testing.T) {
	model := braintest.NewModel(t, braintest.Call("web_search", map[string]any{"query": "笔记"}),
		braintest.Call("delete_note", map[string]any{"ids": []any{3}}), braintest.Say("要删掉第 3 条吗？"), braintest.Say("删好了。"))
	tools := &gateToolbox{}
	f := newChatFixtureWith(t, model, tools)
	key := f.adminKey.AccessToken
	_, created := f.call(t, "POST", "/api/v1/conversations", key, `{}`)
	id := created["conversation_id"].(string)

	status, events := f.stream(t, id, key, `{"text":"把第 3 条笔记删了"}`)
	var kinds []string
	var confirmation map[string]any
	for _, event := range events {
		kinds = append(kinds, event["type"].(string))
		if event["type"] == "tool_started" && event["label"] == "搜索：笔记" && event["call_id"] == "" {
			t.Error("工具开始要带调用编号")
		}
		if event["type"] == "confirmation" {
			confirmation = event["confirmation"].(map[string]any)
		}
	}
	if status != 200 || !slices.Contains(kinds, "tool_started") || !slices.Contains(kinds, "tool_finished") || confirmation == nil ||
		confirmation["action_id"] != "act_1" || confirmation["open"] != true || kinds[len(kinds)-1] != "done" {
		t.Fatalf("事件流：%d %v", status, events)
	}
	if slices.Contains(tools.called(), "delete_note") {
		t.Fatal("点确认之前不应执行删除")
	}
	blocks := func() []any {
		_, page := f.call(t, "GET", "/api/v1/conversations/"+id+"/turns", key, "")
		return page["turns"].([]any)[0].(map[string]any)["reply"].([]any)
	}
	var labels []string
	var open any
	for _, block := range blocks() {
		block := block.(map[string]any)
		if block["type"] == "tool" {
			labels = append(labels, block["label"].(string))
		}
		if block["type"] == "confirmation" {
			open = block["confirmation"].(map[string]any)["open"]
		}
	}
	if len(labels) != 2 || labels[0] != "搜索：笔记" || open != true {
		t.Errorf("记录里的工具说明、确认卡片：%v %v", labels, blocks())
	}

	path := "/api/v1/conversations/" + id + "/confirmations/act_1"
	if status, _ := f.events(t, path, key, `{"decision":"maybe"}`); status != 400 {
		t.Errorf("决定不对：%d", status)
	}
	if status, _ := f.events(t, "/api/v1/conversations/nope/confirmations/act_1", key, `{"decision":"confirm"}`); status != 404 {
		t.Errorf("没有这条对话：%d", status)
	}
	status, events = f.events(t, path, key, `{"decision":"confirm"}`)
	if status != 200 || events[len(events)-1]["type"] != "done" {
		t.Fatalf("点确认：%d %v", status, events)
	}
	if got := tools.called(); !slices.Equal(got, []string{"web_search", "delete_note"}) {
		t.Errorf("点了确认才执行删除：%v", got)
	}
	for _, block := range blocks() {
		if block := block.(map[string]any); block["type"] == "confirmation" && block["confirmation"].(map[string]any)["open"] != false {
			t.Errorf("确认过的卡片不能再点：%v", block)
		}
	}
}
