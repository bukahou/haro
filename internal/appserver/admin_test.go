package appserver

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/money"
)

// storedCaps 是上限从账号库读的假花费（别的同 fakeSpending）：测得到改的上限真的存下了。
type storedCaps struct {
	fakeSpending
	store *accounts.Store
}

func (s storedCaps) AccountCap(ctx context.Context, account string) (money.Amount, error) {
	if account == "" {
		return money.Amount{}, nil
	}
	return s.store.MonthlyCapOf(ctx, account)
}

// 管理员：生成邀请码（码只给这次）→ 列出 → 撤销；在等的账号批准 → 停用（它的令牌作废）→ 恢复 → 删除（彻底删的时间）→ 撤销删除；
// 管理员自己停用 409；不是管理员 403。
func TestAdminOverHTTP(t *testing.T) {
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, _ := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: brain.SharedPerson})
	pair, _ := store.IssueTokens(ctx, admin.ID)
	handler, err := NewHandler(ctx, Options{Accounts: store, Admin: store, Spending: storedCaps{store: store}, DefaultCurrency: "JPY",
		MinAppVersion: "1.0.0", ProfileMaxChars: 10000, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := fixture{server: server, store: store, admin: admin, adminKey: pair}
	key := pair.AccessToken

	status, created := f.call(t, "POST", "/api/v1/admin/invites", key, `{"note":"给妈妈","expires_in_hours":48}`)
	invite, _ := created["invite"].(map[string]any)
	if status != 201 || len(created["code"].(string)) != 14 || invite["status"] != "unused" || created["share_url"] != nil {
		t.Fatalf("生成邀请码：%d %v", status, created)
	}
	if status, _ := f.call(t, "POST", "/api/v1/admin/invites", key, `{"expires_in_hours":1000}`); status != 400 {
		t.Errorf("有效期太长：%d", status)
	}
	if listed := f.list(t, "/api/v1/admin/invites", "zh"); len(listed) != 1 || listed[0]["note"] != "给妈妈" || listed[0]["code"] != nil {
		t.Errorf("列出（不带码）：%v", listed)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/admin/invites/"+invite["invite_id"].(string), key, ""); status != 204 {
		t.Errorf("撤销：%d", status)
	}
	if listed := f.list(t, "/api/v1/admin/invites", "zh"); listed[0]["status"] != "revoked" {
		t.Errorf("撤销后：%v", listed)
	}

	if _, err := store.ResolveLogin(ctx, "sub-guest", "客人", ""); err == nil {
		t.Fatal("没带码应在等")
	}
	guest, _ := store.BySubject(ctx, "sub-guest")
	path := "/api/v1/admin/accounts/" + guest.ID
	if status, body := f.call(t, "POST", path+"/approve", key, ""); status != 200 || body["status"] != "active" {
		t.Fatalf("批准：%d %v", status, body)
	}
	guestPair, _ := store.IssueTokens(ctx, guest.ID)
	if status, _ := f.call(t, "GET", "/api/v1/admin/accounts", guestPair.AccessToken, ""); status != 403 {
		t.Errorf("不是管理员：%d", status)
	}
	if status, body := f.call(t, "POST", path+"/disable", key, ""); status != 200 || body["status"] != "disabled" {
		t.Errorf("停用：%d %v", status, body)
	}
	if status, _ := f.call(t, "GET", "/api/v1/me", guestPair.AccessToken, ""); status != 401 {
		t.Errorf("停用后令牌作废：%d", status)
	}
	if status, body := f.call(t, "POST", path+"/enable", key, ""); status != 200 || body["status"] != "active" {
		t.Errorf("恢复：%d %v", status, body)
	}
	if status, body := f.call(t, "DELETE", path, key, ""); status != 200 || body["deletion_scheduled_at"] == nil || body["status"] != "disabled" {
		t.Errorf("删除：%d %v", status, body)
	}
	if status, body := f.call(t, "POST", path+"/enable", key, ""); status != 200 || body["deletion_scheduled_at"] != nil {
		t.Errorf("撤销删除：%d %v", status, body)
	}
	if status, body := f.call(t, "POST", path+"/approve", key, ""); status != 409 || body["code"] != "conflict" {
		t.Errorf("不在等的批准：%d %v", status, body)
	}
	if status, _ := f.call(t, "POST", "/api/v1/admin/accounts/"+admin.ID+"/disable", key, ""); status != 409 {
		t.Errorf("停用管理员：%d", status)
	}
	if status, body := f.call(t, "PATCH", path, key, `{"monthly_cap":{"amount":"1000","currency":"JPY"}}`); status != 200 ||
		body["cap"].(map[string]any)["amount"] != "1000" {
		t.Errorf("定月上限：%d %v", status, body)
	}
	if got, _ := store.MonthlyCapOf(ctx, guest.ID); got.Micros != 1000*1_000_000 || got.Currency != "JPY" {
		t.Errorf("存下的上限：%+v", got)
	}
	for _, bad := range []string{`{"monthly_cap":{"amount":"10","currency":"USD"}}`, `{"monthly_cap":{"amount":"0","currency":"JPY"}}`,
		`{"monthly_cap":{"amount":"abc","currency":"JPY"}}`} {
		if status, _ := f.call(t, "PATCH", path, key, bad); status != 400 {
			t.Errorf("%s：%d", bad, status)
		}
	}
	if status, body := f.call(t, "PATCH", path, key, `{"monthly_cap":null}`); status != 200 || body["cap"] != nil {
		t.Errorf("不设上限：%d %v", status, body)
	}
	if status, _ := f.call(t, "PATCH", "/api/v1/admin/accounts/"+admin.ID, key, `{"monthly_cap":{"amount":"1000","currency":"JPY"}}`); status != 409 {
		t.Errorf("管理员用全局的上限：%d", status)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/admin/accounts/没有", key, ""); status != 404 {
		t.Errorf("没有的账号：%d", status)
	}
}

// 发给对方的登录地址：有网页版时 = 接口的登录入口 + client=web + 邀请码；没有网页版时没有。
func TestInviteShareURL(t *testing.T) {
	server := &Server{opts: Options{WebURL: "https://web.example.com", Login: &LoginOptions{PublicURL: "https://api.example.com/"}}}
	if got := server.inviteShareURL("ABCD-EFGH-JKMN"); got == nil ||
		*got != "https://api.example.com/api/v1/auth/oidc/start?client=web&invite=ABCD-EFGH-JKMN" {
		t.Errorf("有网页版：%v", got)
	}
	server.opts.WebURL = ""
	if got := server.inviteShareURL("ABCD-EFGH-JKMN"); got != nil {
		t.Errorf("没有网页版：%v", *got)
	}
}
