// Package appserver 是 App 接口的服务端（契约 api/openapi.yaml，接口代码由 internal/appapi 生成）。
//
// 入口的顺序（G9）：先限量（请求体上限、限流）→ 再按契约校验请求、对要登录的接口验令牌 → 再做事。
// 每个请求先解析成账号，接口只用这个账号的「人」取数据（隔离只在这一处做，见 internal/accounts）。
package appserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	middleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/spending"
)

// maxBodyBytes 是请求体的上限（这一批的接口都是小的 JSON；以后传图片的接口另定）。
const maxBodyBytes = 64 << 10

// Accounts 是接口要用的账号库（internal/accounts.Store 实现）。
type Accounts interface {
	Authenticate(ctx context.Context, accessToken string) (accounts.Account, error)
	Refresh(ctx context.Context, refreshToken string) (accounts.TokenPair, error)
	Revoke(ctx context.Context, refreshToken string) error
	IssueTokens(ctx context.Context, accountID string) (accounts.TokenPair, error)
	ResolveLogin(ctx context.Context, subject, displayName string) (accounts.Account, error)
	ByID(ctx context.Context, id string) (accounts.Account, error)
	List(ctx context.Context) ([]accounts.Account, error)
	Touch(ctx context.Context, id, appVersion string) error
	UpdateProfile(ctx context.Context, id, displayName, currency string) (accounts.Account, error)
}

// Spending 是花费的汇总（spending.Guard 实现）。
type Spending interface {
	Summary(ctx context.Context) (spending.Summary, error)
}

// Memory 是某个人记着的事有几条（常驻的、一般的）。
type Memory interface {
	Counts(ctx context.Context, person string) (resident, notes int, err error)
}

// Terminals 是现在在线的终端（brainserver.OnlineTerminals 实现）。
type Terminals interface {
	Count() (online, needsUpgrade int)
}

// Research 是某个人进行中（排队、在查）的调查有几个。
type Research interface {
	Running(ctx context.Context, person string) (int, error)
}

// Options 是接口服务的依赖和设置。Memory、Research 可以是 nil（没开那组工具）。
type Options struct {
	Accounts        Accounts
	Spending        Spending
	Memory          Memory
	Research        Research
	Terminals       Terminals     // nil = 不知道（不显示在线的终端）
	DefaultCurrency string        // 服务端的默认货币（账号没改时用）
	MinAppVersion   string        // 支持的最低 App 版本
	CapOptions      []float64     // 管理员定月上限的可选档位（默认货币）
	ProfileMaxChars int           // 主人资料最多多少字
	Login           *LoginOptions // nil = 不开登录（测试）
	Logger          *slog.Logger
}

// Server 实现 appapi.StrictServerInterface。
type Server struct {
	opts     Options
	accounts Accounts
	texts    *texts
	spec     *openapi3.T
	logger   *slog.Logger
}

var _ appapi.StrictServerInterface = (*Server)(nil)

// NewHandler 建 App 接口的 http.Handler（挂在 /v1/ 下）。
func NewHandler(ctx context.Context, opts Options) (http.Handler, error) {
	loadedTexts, err := loadTexts()
	if err != nil {
		return nil, err
	}
	spec, err := appapi.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("读契约: %w", err)
	}
	spec.Servers = nil // 按路径匹配，不按契约里占位的地址
	server := &Server{opts: opts, accounts: opts.Accounts, texts: loadedTexts, spec: spec, logger: opts.Logger}

	mux := http.NewServeMux()
	limiter := newRateLimiter(ctx)
	if opts.Login != nil {
		login, err := server.newLogin(ctx, *opts.Login)
		if err != nil {
			return nil, err
		}
		mux.Handle("GET /v1/auth/oidc/start", limiter.wrap(http.HandlerFunc(login.start), server))
		mux.Handle("GET /v1/auth/oidc/callback", limiter.wrap(http.HandlerFunc(login.callback), server))
	}
	strict := appapi.NewStrictHandlerWithOptions(server, nil, appapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			server.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": err.Error()})
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			server.logger.Error("App 接口出错", "path", r.URL.Path, "err", err)
			server.writeProblem(w, r, http.StatusInternalServerError, appapi.ErrorCodeInternal, nil)
		},
	})
	validator := middleware.OapiRequestValidatorWithOptions(spec, &middleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: server.authenticate, MultiError: false},
		ErrorHandlerWithOpts: func(ctx context.Context, err error, w http.ResponseWriter, r *http.Request,
			opts middleware.ErrorHandlerOpts) {
			server.rejectRequest(w, r, opts.StatusCode, err)
		},
	})
	generated := appapi.HandlerWithOptions(strict, appapi.StdHTTPServerOptions{BaseRouter: http.NewServeMux()})
	mux.Handle("/v1/", limiter.wrap(validator(generated), server))
	return server.outermost(mux), nil
}

// outermost 是最外层：请求体上限、放一个空的 caller。
func (s *Server) outermost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		r, _ = withCaller(r, s.texts.pick(r.Header.Get("Accept-Language")))
		next.ServeHTTP(w, r)
	})
}

// rejectRequest 是按契约校验没过的请求（status 是校验中间件建议的状态）：没登录 / 令牌不对的给 401（错误码按原因），
// 没有这个接口的给 404，其余的给 400。
func (s *Server) rejectRequest(w http.ResponseWriter, r *http.Request, status int, err error) {
	var securityErr *openapi3filter.SecurityRequirementsError
	if errors.As(err, &securityErr) {
		holder, _ := r.Context().Value(callerKey{}).(*caller)
		cause := errNoBearer
		if holder != nil && holder.err != nil {
			cause = holder.err
		}
		if errors.Is(cause, errNoBearer) {
			s.writeProblem(w, r, http.StatusUnauthorized, appapi.ErrorCodeUnauthorized, nil)
			return
		}
		status, code := accountProblem(cause)
		if status == http.StatusInternalServerError {
			s.logger.Error("验令牌出错", "err", cause)
		}
		s.writeProblem(w, r, status, code, nil)
		return
	}
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		s.writeProblem(w, r, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
		return
	case http.StatusInternalServerError:
		s.logger.Error("校验请求出错", "path", r.URL.Path, "err", err)
		s.writeProblem(w, r, http.StatusInternalServerError, appapi.ErrorCodeInternal, nil)
		return
	}
	s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed,
		map[string]string{"detail": firstLine(err.Error())})
}

// firstLine 只留错误说明的第一行（校验库的说明后面会带整段 schema，不给 App 看）。
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// serverVersion 是服务端的版本（client-config 给 App 看）。
func serverVersion() string { return buildinfo.Version }
