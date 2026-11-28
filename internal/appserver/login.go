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

// 登录：照 melete，haro 后端是唯一的 akasha 客户端（机密客户端），用 akasha 的 oidcrp 走授权码 + PKCE。
// App 用系统的网页登录框打开 /v1/auth/oidc/start；登录完 haro 换成自己的令牌，跳回固定的 nativeCallback。
//
// 回跳只认 nativeCallback 这一个固定地址：⛔ 不回显请求里带来的地址，也不用 oidcrp 自带的回跳检查。
// oidcrp 只会把「next」原样带过登录流程，邀请码借它带过去：next = "/invite/<码>"（inviteNext 只放行这一种写法）。
const (
	nativeCallback = "haro://auth/callback"
	callbackPath   = "/v1/auth/oidc/callback"
	cookiePath     = "/v1/auth/oidc"
	invitePrefix   = "/invite/"
)

// invitePattern 是邀请码的写法（和契约 startLogin 的 invite 参数一致：最长 64 个字）。
var invitePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// LoginOptions 是登录要的 akasha 设置（凭证只来自配置）。
type LoginOptions struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	CookieSecret string // 给登录流程的状态 cookie 签名
	PublicURL    string // App 连的公网根地址（回调 = 它 + callbackPath）
}

type login struct {
	flow   *oidcrp.Flow
	server *Server
}

func (s *Server) newLogin(ctx context.Context, opts LoginOptions) (*login, error) {
	handler := &login{server: s}
	flow, err := oidcrp.New(ctx, oidcrp.Config{
		IssuerURL:       opts.Issuer,
		ClientID:        opts.ClientID,
		ClientSecret:    opts.ClientSecret,
		RedirectURL:     strings.TrimRight(opts.PublicURL, "/") + callbackPath,
		CookieSecret:    opts.CookieSecret,
		CookiePrefix:    "haro_login_",
		CookiePath:      cookiePath,
		CookieSecure:    strings.HasPrefix(opts.PublicURL, "https://"),
		SafeNext:        inviteNext,
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

// inviteNext 是 oidcrp 的 SafeNext：只放行空的、或「/invite/<邀请码>」。
func inviteNext(next string) bool {
	code, ok := strings.CutPrefix(next, invitePrefix)
	return next == "" || (ok && invitePattern.MatchString(code))
}

// start 开始登录：把 invite 参数换成 oidcrp 认的 next，再交给它（跳到 akasha）。写错的邀请码当没带。
func (l *login) start(w http.ResponseWriter, r *http.Request) {
	query := url.Values{}
	if code := r.URL.Query().Get("invite"); invitePattern.MatchString(code) {
		query.Set("next", invitePrefix+code)
	}
	r.URL.RawQuery = query.Encode()
	l.flow.Start(w, r)
}

func (l *login) callback(w http.ResponseWriter, r *http.Request) { l.flow.Callback(w, r) }

// authenticated 是 akasha 登录成功、身份已验签之后：找账号、发 haro 自己的令牌，跳回 App。
// 账号等同意、停用时也跳回 App（带错误码），⛔ 不返回 error —— 那样 App 只会收到笼统的「失败」。
func (l *login) authenticated(_ http.ResponseWriter, r *http.Request, result oidcrp.Result) (string, error) {
	ctx := r.Context()
	identity := result.Identity
	name := identity.Name
	if name == "" {
		name = identity.PreferredUsername
	}
	account, err := l.server.accounts.ResolveLogin(ctx, identity.Subject, name)
	switch {
	case errors.Is(err, accounts.ErrPendingApproval):
		// 管理员要先在配置里写上自己的 sub（HARO_ADMIN_SUBJECT）：第一次登录时从这行日志里取
		l.server.logger.Warn("App 登录：账号在等管理员同意", "subject", identity.Subject, "name", name,
			"invite", strings.TrimPrefix(result.Next, invitePrefix) != "")
		return callbackError(appapi.OidcErrorPendingApproval), nil
	case errors.Is(err, accounts.ErrDisabled):
		return callbackError(appapi.OidcErrorAccountDisabled), nil
	case err != nil:
		return "", err
	}
	pair, err := l.server.accounts.IssueTokens(ctx, account.ID)
	if err != nil {
		return "", err
	}
	_ = l.server.accounts.Touch(ctx, account.ID, appVersion(r.UserAgent()))
	l.server.logger.Info("App 登录", "account", account.ID, "role", account.Role)
	fragment := url.Values{"access_token": {pair.AccessToken}, "refresh_token": {pair.RefreshToken},
		"expires_in": {strconv.Itoa(int(pair.ExpiresIn.Seconds()))}, "token_type": {string(appapi.Bearer)}}
	return nativeCallback + "#" + fragment.Encode(), nil
}

// failed 是登录没成：按 oidcrp 的原因码给 App 稳定的错误码，跳回 App（不让系统的登录框停在网页上）。
func (l *login) failed(w http.ResponseWriter, r *http.Request, reason, _ string) {
	code := appapi.OidcErrorLoginFailed
	if reason == oidcrp.ReasonCancelled {
		code = appapi.OidcErrorLoginCancelled
	}
	l.server.logger.Warn("App 登录没成", "reason", reason)
	http.Redirect(w, r, callbackError(code), http.StatusFound)
}

func callbackError(code appapi.OidcError) string {
	return nativeCallback + "?" + url.Values{"oidc_error": {string(code)}}.Encode()
}
