package appserver

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/bukahou/haro/internal/accounts"
)

// fakeOnline：t-online 在线；记下断开了哪台。
type fakeOnline struct{ disconnected []string }

func (f *fakeOnline) Unidentified() (int, int) { return 1, 0 }
func (f *fakeOnline) Status(id string) (bool, string) {
	if id == "t-online" {
		return true, "v0.0.1"
	}
	return false, ""
}
func (f *fakeOnline) Disconnect(id string) { f.disconnected = append(f.disconnected, id) }

func newTerminalFixture(t *testing.T) (fixture, *fakeOnline) {
	t.Helper()
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, err := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: "_shared"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.IssueTokens(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	online := &fakeOnline{}
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY", MinAppVersion: "1.0.0",
		ProfileMaxChars: 10000, Terminals: online, Registry: store, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return fixture{server: server, store: store, admin: admin, adminKey: pair}, online
}

// 配对：输入终端显示的配对码 → 绑到这个账号、列表里有；用过的再输是 409；改名；移除 → 凭证作废、正连着的断开。
func TestPairRenameRemoveTerminal(t *testing.T) {
	f, online := newTerminalFixture(t)
	ctx := t.Context()
	key := f.adminKey.AccessToken
	registration, err := f.store.StartRegistration(ctx, "客厅", "v0.0.1", "192.168.1.20", "")
	if err != nil {
		t.Fatal(err)
	}
	status, body := f.call(t, "POST", "/api/v1/terminals/pair", key, fmt.Sprintf(`{"code":%q}`, registration.Code))
	terminal, _ := body["terminal"].(map[string]any)
	if status != 200 || terminal["name"] != "客厅" || terminal["network"] != "lan" {
		t.Fatalf("配对：%d %v", status, body)
	}
	id := terminal["terminal_id"].(string)
	if status, body := f.call(t, "POST", "/api/v1/terminals/pair", key, fmt.Sprintf(`{"code":%q}`, registration.Code)); status != 409 ||
		body["code"] != "conflict" {
		t.Errorf("用过的配对码：%d %v", status, body)
	}
	if items := f.list(t, "/api/v1/terminals", "zh"); len(items) != 1 || items[0]["terminal_id"] != id || items[0]["online"] != false {
		t.Errorf("列表：%v", items)
	}
	if status, body := f.call(t, "PATCH", "/api/v1/terminals/"+id, key, `{"name":"卧室"}`); status != 200 || body["name"] != "卧室" {
		t.Errorf("改名：%d %v", status, body)
	}
	if status, _ := f.call(t, "GET", "/api/v1/me/overview", key, ""); status != 200 {
		t.Errorf("概览：%d", status)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/terminals/"+id, key, ""); status != 204 {
		t.Errorf("移除：%d", status)
	}
	if len(online.disconnected) != 1 || online.disconnected[0] != id {
		t.Errorf("移除后要断开它：%v", online.disconnected)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/terminals/"+id, key, ""); status != 404 {
		t.Errorf("再移除一次：%d", status)
	}
}

// 配对码输错：400 pairing_code_invalid；同一个账号输错 5 次后（换着来源也算）429 pairing_attempts_exceeded。
func TestPairingAttemptsPerAccount(t *testing.T) {
	f, _ := newTerminalFixture(t)
	key := f.adminKey.AccessToken
	for i := range pairFailureLimit {
		if status, body := f.call(t, "POST", "/api/v1/terminals/pair", key, `{"code":"AAAA-AAAA"}`,
			"CF-Connecting-IP", fmt.Sprintf("203.0.113.%d", i+1)); status != 400 || body["code"] != "pairing_code_invalid" {
			t.Fatalf("第 %d 次输错：%d %v", i+1, status, body)
		}
	}
	if status, body := f.call(t, "POST", "/api/v1/terminals/pair", key, `{"code":"AAAA-AAAA"}`, "CF-Connecting-IP", "203.0.113.99"); status != 429 ||
		body["code"] != "pairing_attempts_exceeded" {
		t.Errorf("输错太多次：%d %v", status, body)
	}
}

// 新终端：管理员看得到（短编号、局域网 / 外网）、批准 = 匿名账号（终端轮询拿到凭证）、拒绝；不是管理员 403。
func TestPendingTerminalsForAdmin(t *testing.T) {
	f, _ := newTerminalFixture(t)
	ctx := t.Context()
	key := f.adminKey.AccessToken
	approved, _ := f.store.StartRegistration(ctx, "书房", "v0.0.1", "203.0.113.7", "")
	rejected, _ := f.store.StartRegistration(ctx, "门口", "v0.0.1", "192.168.1.30", "")
	pending := f.list(t, "/api/v1/admin/terminals/pending", "zh")
	networks := map[string]any{}
	for _, item := range pending {
		networks[item["reported_name"].(string)] = item["network"]
	}
	if len(pending) != 2 || networks["书房"] != "internet" || networks["门口"] != "lan" || pending[0]["short_id"] == "" {
		t.Fatalf("新终端：%v", pending)
	}
	if status, _ := f.call(t, "POST", "/api/v1/admin/terminals/pending/"+approved.ID+"/approve", key, ""); status != 204 {
		t.Errorf("批准：%d", status)
	}
	if result, err := f.store.PollRegistration(ctx, approved.PollToken); err != nil || result.Credential == "" {
		t.Errorf("批准后终端拿到凭证：%+v %v", result, err)
	}
	if status, _ := f.call(t, "POST", "/api/v1/admin/terminals/pending/"+rejected.ID+"/reject", key, ""); status != 204 {
		t.Errorf("拒绝：%d", status)
	}
	if status, _ := f.call(t, "POST", "/api/v1/admin/terminals/pending/"+rejected.ID+"/approve", key, ""); status != 404 {
		t.Errorf("拒绝过的不能再批准：%d", status)
	}
	// 匿名账号（批准出来的）是普通账号：看不到新终端
	_, anonymous, err := f.store.AuthenticateTerminal(ctx, mustCredential(t, f, "阳台"))
	if err != nil {
		t.Fatal(err)
	}
	memberKey, err := f.store.IssueTokens(ctx, anonymous.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.call(t, "GET", "/api/v1/admin/terminals/pending", memberKey.AccessToken, ""); status != 403 || body["code"] != "forbidden" {
		t.Errorf("不是管理员：%d %v", status, body)
	}
}

// mustCredential 登记一台终端、管理员批准、终端取走凭证。
func mustCredential(t *testing.T, f fixture, name string) string {
	t.Helper()
	ctx := t.Context()
	registration, err := f.store.StartRegistration(ctx, name, "v0.0.1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApproveRegistration(ctx, registration.ID); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.PollRegistration(ctx, registration.PollToken)
	if err != nil {
		t.Fatal(err)
	}
	return result.Credential
}
