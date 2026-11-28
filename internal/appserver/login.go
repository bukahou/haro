package appserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bukahou/akasha/pkg/oidcrp"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
)

// 登录：haro 后端是唯一的 akasha 客户端（机密客户端），用 akasha 的 oidcrp 走授权码 + PKCE。
// App 用系统的网页登录框、网页版用浏览器打开 startPath；登录完 haro 换成自己的令牌，跳回发起的一方：
// App 是固定的 nativeCallback，网页版是它自己的回调页（Options.WebURL + webCallbackPath）。
//
// 回跳只认这两个固定地址：⛔ 不回显请求里带来的地址，也不用 oidcrp 自带的回跳检查。
// oidcrp 只会把「next」原样带过登录流程：回哪一方、邀请码编进 next（loginTarget），SafeNext 只放行 loginTarget 自己编出来的写法。
const (
	nativeCallback  = "haro://auth/callback"
	webCallbackPath = "/auth/callback" // 网页版接登录结果的页面（web/ 里同名的路由）
	startPath       = PathPrefix + "auth/oidc/start"
	callbackPath    = PathPrefix + "auth/oidc/callback"
	cookiePath      = PathPrefix + "auth/oidc"
)

// startLogin 的 client 参数（和契约一致，测试守着）：不带 = App。
const (
	clientApp = "app"
	clientWeb = "web"
)

var loginClients = []string{clientApp, clientWeb}

// invitePattern 是邀请码的写法（和契约 startLogin 的 invite 参数一致：最长 64 个字）。
var invitePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// LoginOptions 是登录要的 akasha 设置（凭证只来自配置）。网页版的地址在 Options.WebURL。
type LoginOptions struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	CookieSecret string // 给登录流程的状态 cookie 签名
	PublicURL    string // 接口的公网根地址（回调 = 它 + callbackPath）
}

type login struct {
	flow        *oidcrp.Flow
	server      *Server
	webCallback string // 网页版的回调页；空 = 没有网页版
}

// loginTarget 是登录完回哪一方、带没带邀请码：编成 next 带过登录流程。
type loginTarget struct {
	web    bool
	invite string
}

// next 编成 oidcrp 带过登录流程的 next：App、没邀请码时为空，否则是「/?client=web&invite=<码>」这样的本站地址。
func (t loginTarget) next() string {
	query := url.Values{}
	if t.web {
		query.Set("client", clientWeb)
	}
	if t.invite != "" {
		query.Set("invite", t.invite)
	}
	if len(query) == 0 {
		return ""
	}
	return (&url.URL{Path: "/", RawQuery: query.Encode()}).String()
}

// parseLoginTarget 解出 next；只认 loginTarget.next 编出来的写法（多余的参数、换了顺序、别的路径都不认）。
func parseLoginTarget(next string) (loginTarget, bool) {
	if next == "" {
		return loginTarget{}, true
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" || parsed.Path != "/" {
		return loginTarget{}, false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return loginTarget{}, false
	}
	target := loginTarget{web: query.Get("client") == clientWeb, invite: query.Get("invite")}
	if target.invite != "" && !invitePattern.MatchString(target.invite) {
		return loginTarget{}, false
	}
	return target, target.next() == next
}

// safeNext 是 oidcrp 的 SafeNext。
func safeNext(next string) bool {
	_, ok := parseLoginTarget(next)
	return ok
}

func (s *Server) newLogin(ctx context.Context, opts LoginOptions) (*login, error) {
	handler := &login{server: s}
	if s.opts.WebURL != "" {
		webCallback, err := url.JoinPath(s.opts.WebURL, webCallbackPath)
		if err != nil {
			return nil, fmt.Errorf("网页版的地址 %q: %w", s.opts.WebURL, err)
		}
		handler.webCallback = webCallback
	}
	flow, err := oidcrp.New(ctx, oidcrp.Config{
		IssuerURL:       opts.Issuer,
		ClientID:        opts.ClientID,
		ClientSecret:    opts.ClientSecret,
		RedirectURL:     strings.TrimRight(opts.PublicURL, "/") + callbackPath,
		CookieSecret:    opts.CookieSecret,
		CookiePrefix:    "haro_login_",
		CookiePath:      cookiePath,
		CookieSecure:    strings.HasPrefix(opts.PublicURL, "https://"),
		SafeNext:        safeNext,
		OnAuthenticated: handler.authenticated,
		OnError:         handler.failed,
		Logger:          s.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("准备 akasha 登录（%s）: %w", opts.Issuer, err)
	}
	handler.flow = flow
	return handler, nil
}

// start 开始登录：client、invite 参数编成 oidcrp 认的 next，再交给它（跳到 akasha）。写错的邀请码当没带；
// client 不认识的给 400，要网页版而这里没有网页版的给 404。
func (l *login) start(w http.ResponseWriter, r *http.Request) {
	incoming := r.URL.Query()
	target := loginTarget{}
	switch client := incoming.Get("client"); client {
	case "", clientApp:
	case clientWeb:
		if l.webCallback == "" {
			l.server.writeProblem(w, r, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
			return
		}
		target.web = true
	default:
		l.server.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed,
			map[string]string{"detail": "client=" + strconv.Quote(client)})
		return
	}
	if code := incoming.Get("invite"); invitePattern.MatchString(code) {
		target.invite = code
	}
	query := url.Values{}
	if next := target.next(); next != "" {
		query.Set("next", next)
	}
	r.URL.RawQuery = query.Encode()
	l.flow.Start(w, r)
}

func (l *login) callback(w http.ResponseWriter, r *http.Request) { l.flow.Callback(w, r) }

// returnTo 是回跳的地址（后面接「#令牌」或「?oidc_error=」）：网页版发起的回网页的回调页，其余回 App。
func (l *login) returnTo(target loginTarget) string {
	if target.web && l.webCallback != "" {
		return l.webCallback
	}
	return nativeCallback
}

// authenticated 是 akasha 登录成功、身份已验签之后：找账号、发 haro 自己的令牌，跳回发起的一方。
// 账号等同意、停用时也跳回去（带错误码），⛔ 不返回 error —— 那样只会收到笼统的「失败」。
func (l *login) authenticated(_ http.ResponseWriter, r *http.Request, result oidcrp.Result) (string, error) {
	ctx := r.Context()
	target, _ := parseLoginTarget(result.Next) // oidcrp 已经用 safeNext 检查过
	identity := result.Identity
	name := identity.Name
	if name == "" {
		name = identity.PreferredUsername
	}
	account, err := l.server.accounts.ResolveLogin(ctx, identity.Subject, name, target.invite)
	switch {
	case errors.Is(err, accounts.ErrInviteInvalid):
		l.server.logger.Warn("App 登录：邀请码不对、过期了或者用过了，没建账号", "web", target.web)
		return l.callbackError(target, appapi.OidcErrorInviteInvalid), nil
	case errors.Is(err, accounts.ErrPendingApproval):
		// 管理员要先在配置里写上自己的 sub（HARO_ADMIN_SUBJECT）：第一次登录时从这行日志里取
		l.server.logger.Warn("App 登录：账号在等管理员同意", "subject", identity.Subject, "name", name,
			"invite", target.invite != "", "web", target.web)
		return l.callbackError(target, appapi.OidcErrorPendingApproval), nil
	case errors.Is(err, accounts.ErrDisabled):
		return l.callbackError(target, appapi.OidcErrorAccountDisabled), nil
	case err != nil:
		return "", err
	}
	pair, err := l.server.accounts.IssueTokens(ctx, account.ID)
	if err != nil {
		return "", err
	}
	_ = l.server.accounts.Touch(ctx, account.ID, appVersion(r.UserAgent()))
	l.server.logger.Info("App 登录", "account", account.ID, "role", account.Role, "web", target.web)
	fragment := url.Values{"access_token": {pair.AccessToken}, "refresh_token": {pair.RefreshToken},
		"expires_in": {strconv.Itoa(int(pair.ExpiresIn.Seconds()))}, "token_type": {string(appapi.Bearer)}}
	return l.returnTo(target) + "#" + fragment.Encode(), nil
}

// failed 是登录没成：按 oidcrp 的原因码给稳定的错误码，跳回发起的一方（不让 App 的登录框停在网页上）。
func (l *login) failed(w http.ResponseWriter, r *http.Request, reason, next string) {
	target, _ := parseLoginTarget(next)
	code := appapi.OidcErrorLoginFailed
	if reason == oidcrp.ReasonCancelled {
		code = appapi.OidcErrorLoginCancelled
	}
	l.server.logger.Warn("App 登录没成", "reason", reason, "web", target.web)
	http.Redirect(w, r, l.callbackError(target, code), http.StatusFound)
}

func (l *login) callbackError(target loginTarget, code appapi.OidcError) string {
	return l.returnTo(target) + "?" + url.Values{"oidc_error": {string(code)}}.Encode()
}
