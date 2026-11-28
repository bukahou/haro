package appserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
)

// 哪些接口要登录写在契约里（security）：按契约校验请求的中间件对要登录的接口调 authenticate，
// 验过的账号放进这次请求的 caller 里，接口从 callerFrom 取 —— 「哪些要登录」只在契约里写一处。

type callerKey struct{}

// caller 是这次请求是谁（验令牌时填上）。
type caller struct {
	language string // 给人看的文字用的语言（按 Accept-Language 选好的）
	account  accounts.Account
	err      error // 验令牌失败的原因（中间件按它给状态和错误码）
	checked  bool
}

// withCaller 给这次请求放一个空的 caller（中间件最外层），先选好语言。
func withCaller(r *http.Request, language string) (*http.Request, *caller) {
	holder := &caller{language: language}
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, holder)), holder
}

// callerFrom 取这次请求验过的账号；没验过（不该发生：契约要求登录的接口才会走到这里）时 ok = false。
func callerFrom(ctx context.Context) (accounts.Account, bool) {
	holder, _ := ctx.Value(callerKey{}).(*caller)
	if holder == nil || !holder.checked || holder.err != nil {
		return accounts.Account{}, false
	}
	return holder.account, true
}

// errNoBearer 是没带令牌、或者不是 Bearer。
var errNoBearer = errors.New("没带 Bearer 令牌")

// authenticate 是按契约校验请求时，对要登录的接口验令牌（openapi3filter.AuthenticationFunc）。
func (s *Server) authenticate(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
	holder, _ := ctx.Value(callerKey{}).(*caller)
	if holder == nil {
		return errors.New("请求里没有 caller（中间件的顺序不对）")
	}
	holder.checked = true
	header := input.RequestValidationInput.Request.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found || strings.TrimSpace(token) == "" {
		holder.err = errNoBearer
		return holder.err
	}
	account, err := s.accounts.Authenticate(ctx, strings.TrimSpace(token))
	if err != nil {
		holder.err = err
		return err
	}
	holder.account = account
	_ = s.accounts.Touch(ctx, account.ID, appVersion(input.RequestValidationInput.Request.UserAgent()))
	return nil
}

// appVersion 从 User-Agent 里取 App 的版本（约定的写法是「haro-ios/1.0.0」）；认不出时为空。
func appVersion(userAgent string) string {
	for _, part := range strings.Fields(userAgent) {
		if product, version, ok := strings.Cut(part, "/"); ok && strings.HasPrefix(product, "haro-") && version != "" {
			return product + "/" + version
		}
	}
	return ""
}

// languageFrom 是这次请求选好的语言（没有 caller 时为空：texts 按默认语言）。
func languageFrom(ctx context.Context) string {
	if holder, _ := ctx.Value(callerKey{}).(*caller); holder != nil {
		return holder.language
	}
	return ""
}

// bearerAccount 是手写挂载、要登录的接口（打字的流）验令牌：和契约里要登录的接口同一套（Authenticate、错误码），
// 不行时已经回好了 401 / 500。
func (s *Server) bearerAccount(w http.ResponseWriter, r *http.Request) (accounts.Account, bool) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || strings.TrimSpace(token) == "" {
		s.writeProblem(w, r, http.StatusUnauthorized, appapi.ErrorCodeUnauthorized, nil)
		return accounts.Account{}, false
	}
	account, err := s.accounts.Authenticate(r.Context(), strings.TrimSpace(token))
	if err != nil {
		status, code := accountProblem(err)
		if status == http.StatusInternalServerError {
			s.logger.Error("验令牌出错", "err", err)
		}
		s.writeProblem(w, r, status, code, nil)
		return accounts.Account{}, false
	}
	_ = s.accounts.Touch(r.Context(), account.ID, appVersion(r.UserAgent()))
	return account, true
}
