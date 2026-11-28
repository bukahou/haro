package appserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bukahou/akasha/pkg/oidcrp"
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/spending"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeSpending struct{}

// SummaryFor：管理员看到的是全局的这一份；别的账号是自己的（这里都是 0、上限 1000）。
func (f fakeSpending) SummaryFor(ctx context.Context, account string) (spending.Summary, error) {
	if account != "" {
		spent, _ := f.AccountSpent(ctx, account)
		return spending.Summary{Currency: "JPY", Month: spent, MonthCap: money.Of(1000, "JPY"), Status: spending.StatusOK,
			TodayResearch: spent}, nil
	}
	return f.summary()
}

func (fakeSpending) AccountSpent(context.Context, string) (spending.Spent, error) {
	return spending.Spent{ByCurrency: money.Sum{}, Total: money.Total{Amount: money.Amount{Currency: "JPY"}}}, nil
}

func (fakeSpending) AccountCap(_ context.Context, account string) (money.Amount, error) {
	if account == "" {
		return money.Amount{}, nil
	}
	return money.Of(1000, "JPY"), nil
}

func (fakeSpending) summary() (spending.Summary, error) {
	month := spending.Spent{ByCurrency: money.Sum{"USD": 2_000_000, "JPY": 100_000_000},
		Total: money.Total{Amount: money.Of(420, "JPY"), Converted: []string{"USD"}}}
	return spending.Summary{Currency: "JPY", Month: month, MonthCap: money.Of(3000, "JPY"), Status: spending.StatusOK,
		CanRaise: true, Raise: money.Of(500, "JPY"), TodayResearch: spending.Spent{ByCurrency: money.Sum{},
			Total: money.Total{Amount: money.Amount{Currency: "JPY"}}}, ResearchCap: money.Of(100, "JPY"), SearchCap: 30}, nil
}

type fixture struct {
	server   *httptest.Server
	store    *accounts.Store
	admin    accounts.Account
	adminKey accounts.TokenPair
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, err := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.IssueTokens(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY",
		MinAppVersion: "1.0.0", CapOptions: []float64{1000, 3000}, ProfileMaxChars: 10000, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return fixture{server: server, store: store, admin: admin, adminKey: pair}
}

// call 发一个请求，返回状态和解出来的 JSON（problem 的 code 在 body["code"]）。
func (f fixture) call(t *testing.T, method, path, token, body string, headers ...string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

// 不用登录的 client-config：服务端版本、契约版本、最低 App 版本；字数上限从契约里取，档位是默认货币。
func TestClientConfig(t *testing.T) {
	f := newFixture(t)
	spec, err := appapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	status, body := f.call(t, "GET", "/api/v1/client-config", "", "")
	if status != 200 || body["api_version"] != spec.Info.Version || body["min_app_version"] != "1.0.0" {
		t.Fatalf("client-config：%d %v", status, body)
	}
	limits := body["limits"].(map[string]any)
	if limits["display_name_max_chars"] != float64(40) || limits["terminal_name_max_chars"] != float64(40) || limits["profile_max_chars"] != float64(10000) {
		t.Errorf("字数上限：%v", limits)
	}
	options := body["monthly_cap_options"].([]any)
	if len(options) != 2 || options[1].(map[string]any)["amount"] != "3000" || options[1].(map[string]any)["currency"] != "JPY" {
		t.Errorf("档位：%v", options)
	}
}

// 要登录的接口：没带令牌 unauthorized、令牌不对 invalid_token（都是 401，按 code 区分）；说明按 Accept-Language。
func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	if status, body := f.call(t, "GET", "/api/v1/me", "", ""); status != 401 || body["code"] != "unauthorized" {
		t.Errorf("没带令牌：%d %v", status, body)
	}
	if status, body := f.call(t, "GET", "/api/v1/me", "wrong", "", "Accept-Language", "ja-JP,ja;q=0.9"); status != 401 ||
		body["code"] != "invalid_token" || !strings.Contains(body["detail"].(string), "ログイン") {
		t.Errorf("令牌不对（日语的说明）：%d %v", status, body)
	}
	status, body := f.call(t, "GET", "/api/v1/me", f.adminKey.AccessToken, "")
	if status != 200 || body["role"] != "admin" || body["default_currency"] != "JPY" || body["account_id"] != f.admin.ID {
		t.Errorf("管理员：%d %v", status, body)
	}
	if status, body := f.call(t, "GET", "/api/v1/nope", f.adminKey.AccessToken, ""); status != 404 || body["code"] != "not_found" {
		t.Errorf("没有的接口：%d %v", status, body)
	}
}

// 请求按契约校验：显示名超过 40 字、币种不是三个大写字母 → 400 validation_failed；改得对就改上。
func TestUpdateMeValidation(t *testing.T) {
	f := newFixture(t)
	long := `{"display_name": "` + strings.Repeat("长", 41) + `"}`
	if status, body := f.call(t, "PATCH", "/api/v1/me", f.adminKey.AccessToken, long); status != 400 || body["code"] != "validation_failed" {
		t.Errorf("显示名太长：%d %v", status, body)
	}
	if status, body := f.call(t, "PATCH", "/api/v1/me", f.adminKey.AccessToken, `{"default_currency": "yen"}`); status != 400 {
		t.Errorf("币种写错：%d %v", status, body)
	}
	if status, body := f.call(t, "PATCH", "/api/v1/me", f.adminKey.AccessToken, `{"display_name": "老板", "default_currency": "CNY"}`); status != 200 ||
		body["display_name"] != "老板" || body["default_currency"] != "CNY" {
		t.Errorf("改显示名、默认货币：%d %v", status, body)
	}
}

// 花费：默认货币的合计、按币种的原始合计、换算提示；上限、剩下多少、能不能口头提高。
func TestSpending(t *testing.T) {
	f := newFixture(t)
	status, body := f.call(t, "GET", "/api/v1/spending", f.adminKey.AccessToken, "")
	if status != 200 {
		t.Fatalf("%d %v", status, body)
	}
	month := body["month"].(map[string]any)
	spent := month["spent"].(map[string]any)
	if spent["total"].(map[string]any)["amount"] != "420" || len(spent["by_currency"].([]any)) != 2 || spent["conversion_note"] == nil ||
		month["remaining"].(map[string]any)["amount"] != "2580" || month["can_raise"] != true || month["used_ratio"] != 0.14 {
		t.Errorf("这个月：%v", month)
	}
	if purposes := body["by_purpose"].([]any); len(purposes) != 0 {
		t.Errorf("假的汇总里没有按用途分开的：%v", purposes)
	}
}

// 管理员才能看账号列表；等同意的账号拿不到令牌能用的接口（登录时就被挡在外面）。
func TestAdminOnly(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.store.ResolveLogin(ctx, "sub-guest", "客人", ""); err == nil {
		t.Fatal("新账号应等同意")
	}
	status, body := f.call(t, "GET", "/api/v1/admin/accounts", f.adminKey.AccessToken, "")
	if status != 200 {
		t.Fatalf("%d %v", status, body)
	}
	status, overview := f.call(t, "GET", "/api/v1/me/overview", f.adminKey.AccessToken, "")
	if status != 200 || overview["admin"].(map[string]any)["pending_accounts"] != float64(1) ||
		overview["account"].(map[string]any)["role"] != "admin" || overview["account"].(map[string]any)["display_name"] != "主人" {
		t.Errorf("概览里管理员看得到等同意的账号数：%d %v", status, overview)
	}
	guest, _ := f.store.BySubject(ctx, "sub-guest")
	guestKey, _ := f.store.IssueTokens(ctx, guest.ID)
	if status, body := f.call(t, "GET", "/api/v1/admin/accounts", guestKey.AccessToken, ""); status != 401 || body["code"] != "pending_approval" {
		t.Errorf("等同意的账号：%d %v", status, body)
	}
}

// 刷新令牌换一对新的；旧的再来用被拒绝（整串作废）；还没做的接口如实回 501。
func TestRefreshAndNotImplemented(t *testing.T) {
	f := newFixture(t)
	status, body := f.call(t, "POST", "/api/v1/auth/refresh", "", `{"refresh_token": "`+f.adminKey.RefreshToken+`"}`)
	if status != 200 || body["token_type"] != "Bearer" || body["expires_in"] != float64(3600) {
		t.Fatalf("刷新：%d %v", status, body)
	}
	if status, body := f.call(t, "POST", "/api/v1/auth/refresh", "", `{"refresh_token": "`+f.adminKey.RefreshToken+`"}`); status != 401 ||
		body["code"] != "invalid_token" {
		t.Errorf("旧的刷新令牌再来用：%d %v", status, body)
	}
	newKey := body["access_token"].(string)
	if status, body := f.call(t, "GET", "/api/v1/terminals", newKey, ""); status != 401 {
		t.Errorf("整串作废后新的访问令牌也不能用：%d %v", status, body)
	}
	pair, _ := f.store.IssueTokens(t.Context(), f.admin.ID)
	if status, body := f.call(t, "GET", "/api/v1/me/profile", pair.AccessToken, ""); status != 501 || body["code"] != "not_implemented" {
		t.Errorf("还没做的接口：%d %v", status, body)
	}
}

// 登录、刷新这类接口按来源限流：一下子来太多 → 429 + Retry-After。
func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	limited := false
	for range 30 {
		if status, _ := f.call(t, "POST", "/api/v1/auth/refresh", "", `{"refresh_token": "x"}`); status == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("一下子 30 次刷新应被限流")
	}
}

// akasha 登录回来后：管理员拿到 haro 的令牌（在固定回调地址的 fragment 里）；新账号等同意（错误码在 query 里）；
// 网页版发起的回网页版的回调页。
func TestLoginOutcome(t *testing.T) {
	f := newFixture(t)
	loadedTexts, _ := loadTexts()
	server := &Server{accounts: f.store, texts: loadedTexts, logger: quietLogger}
	const webCallback = "https://web.example.com/auth/callback"
	handler := &login{server: server, webCallback: webCallback}
	request := httptest.NewRequest("GET", callbackPath, nil)
	to, err := handler.authenticated(nil, request, oidcrp.Result{Identity: oidcrp.Identity{Subject: "sub-owner"}})
	if err != nil || !strings.HasPrefix(to, nativeCallback+"#") {
		t.Fatalf("管理员登录：%q %v", to, err)
	}
	fragment, _ := url.ParseQuery(strings.TrimPrefix(to, nativeCallback+"#"))
	if account, err := f.store.Authenticate(t.Context(), fragment.Get("access_token")); err != nil || account.ID != f.admin.ID {
		t.Errorf("fragment 里的访问令牌能用：%v", err)
	}
	if to, err := handler.authenticated(nil, request, oidcrp.Result{Identity: oidcrp.Identity{Subject: "sub-new"}}); err != nil ||
		to != nativeCallback+"?oidc_error=pending_approval" {
		t.Errorf("新账号：%q %v", to, err)
	}
	web := loginTarget{web: true}.next()
	if to, err := handler.authenticated(nil, request, oidcrp.Result{Identity: oidcrp.Identity{Subject: "sub-owner"}, Next: web}); err != nil ||
		!strings.HasPrefix(to, webCallback+"#access_token=") {
		t.Errorf("网页版登录：%q %v", to, err)
	}
	if to, _ := handler.authenticated(nil, request, oidcrp.Result{Identity: oidcrp.Identity{Subject: "sub-new2"}, Next: web}); to != webCallback+"?oidc_error=pending_approval" {
		t.Errorf("网页版、新账号：%q", to)
	}
	recorder := httptest.NewRecorder()
	handler.failed(recorder, request, oidcrp.ReasonCancelled, web)
	if location := recorder.Header().Get("Location"); location != webCallback+"?oidc_error=login_cancelled" {
		t.Errorf("网页版取消登录回：%q", location)
	}
	noWeb := &login{server: server}
	if to, _ := noWeb.authenticated(nil, request, oidcrp.Result{Identity: oidcrp.Identity{Subject: "sub-owner"}, Next: web}); !strings.HasPrefix(to, nativeCallback+"#") {
		t.Errorf("没有网页版时回 App：%q", to)
	}
}

// next 只认 loginTarget 自己编出来的写法：别的路径、别的主机、多余或重复的参数、写错的邀请码都不认。
func TestLoginTargetNext(t *testing.T) {
	for _, target := range []loginTarget{{}, {web: true}, {invite: "abc-123"}, {web: true, invite: "abc-123"}} {
		if parsed, ok := parseLoginTarget(target.next()); !ok || parsed != target {
			t.Errorf("%+v 编成 %q 再解出来是 %+v %v", target, target.next(), parsed, ok)
		}
	}
	for _, next := range []string{"/invite/abc", "/?client=app", "/?client=web&client=web", "/?client=web&x=1",
		"/?invite=a%20b", "/?invite=", "//evil.example/?client=web", "https://evil.example/?client=web", "/x?client=web",
		"/\t/evil.example", "/?invite=abc&client=web", "/?client=web#x"} {
		if safeNext(next) {
			t.Errorf("safeNext(%q) 应不认", next)
		}
	}
}

// 开始登录：client 不认识的给 400；要网页版而这里没有网页版的给 404（都在跳到 akasha 之前）。契约里 client 的取值和代码一致。
func TestStartLoginClient(t *testing.T) {
	loadedTexts, _ := loadTexts()
	server := &Server{texts: loadedTexts, logger: quietLogger}
	handler := &login{server: server}
	for query, want := range map[string]int{"client=desktop": http.StatusBadRequest, "client=web": http.StatusNotFound} {
		recorder := httptest.NewRecorder()
		handler.start(recorder, httptest.NewRequest("GET", startPath+"?"+query, nil))
		if recorder.Code != want {
			t.Errorf("%s 应回 %d，回了 %d", query, want, recorder.Code)
		}
	}
	// 登录的两个跳转不生成代码（内嵌的契约里也没有），直接读契约文件
	spec, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	item := spec.Paths.Find(startPath)
	if item == nil || item.Get == nil {
		t.Fatal("契约里没有 startLogin")
	}
	parameter := item.Get.Parameters.GetByInAndName("query", "client")
	if parameter == nil {
		t.Fatal("契约的 startLogin 没有 client 参数")
	}
	var enum []string
	for _, value := range parameter.Schema.Value.Enum {
		enum = append(enum, value.(string))
	}
	if strings.Join(enum, ",") != strings.Join(loginClients, ",") {
		t.Errorf("契约里 client 的取值 %v，代码里 %v", enum, loginClients)
	}
}

// 网页版跨域：只放行设好的那一个来源（预检、普通请求、被拒的回答都带头）；别的来源、没有网页版时不带。
func TestCrossOrigin(t *testing.T) {
	store, err := accounts.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	build := func(webURL string) *httptest.Server {
		handler, err := NewHandler(t.Context(), Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY",
			MinAppVersion: "1.0.0", ProfileMaxChars: 10000, WebURL: webURL, Logger: quietLogger})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return server
	}
	const web = "https://web.example.com"
	send := func(server *httptest.Server, method, path, origin string, headers ...string) *http.Response {
		request, _ := http.NewRequest(method, server.URL+path, nil)
		request.Header.Set("Origin", origin)
		for i := 0; i+1 < len(headers); i += 2 {
			request.Header.Set(headers[i], headers[i+1])
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response
	}
	withWeb := build(web)
	preflight := send(withWeb, "OPTIONS", "/api/v1/me", web, "Access-Control-Request-Method", "PATCH",
		"Access-Control-Request-Headers", "authorization,content-type")
	if preflight.Header.Get("Access-Control-Allow-Origin") != web || !strings.Contains(preflight.Header.Get("Access-Control-Allow-Methods"), "PATCH") {
		t.Errorf("预检：%d %v", preflight.StatusCode, preflight.Header)
	}
	if response := send(withWeb, "GET", "/api/v1/me", web); response.StatusCode != http.StatusUnauthorized ||
		response.Header.Get("Access-Control-Allow-Origin") != web {
		t.Errorf("没登录的回答也要带跨域的头：%d %v", response.StatusCode, response.Header)
	}
	if response := send(withWeb, "GET", "/api/v1/client-config", "https://evil.example"); response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("别的来源不放行")
	}
	if response := send(build(""), "GET", "/api/v1/client-config", web); response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("没有网页版时不放行跨域")
	}
}

// App 接口的前缀和终端的入口不能重叠：公网只放行 PathPrefix，重叠了就会把不鉴权的语音连接、声纹登记一起开出去。
// 契约里的每个路径都要在 PathPrefix 下。
func TestPathPrefixDoesNotCoverTerminalRoutes(t *testing.T) {
	for _, path := range []string{realtimeproto.ConversationPath, realtimeproto.VoiceprintPath, realtimeproto.EnrollmentScriptPath} {
		if strings.HasPrefix(path, PathPrefix) || strings.HasPrefix(PathPrefix, path) {
			t.Errorf("终端的入口 %s 和 App 接口的前缀 %s 重叠", path, PathPrefix)
		}
	}
	spec, err := appapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	for path := range spec.Paths.Map() {
		if !strings.HasPrefix(path, PathPrefix) {
			t.Errorf("契约里的 %s 不在 %s 下", path, PathPrefix)
		}
	}
}
