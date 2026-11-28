// Package appserver 是 App 接口的服务端（契约 api/openapi.yaml，接口代码由 internal/appapi 生成）。
//
// 入口的顺序（G9）：先限量（请求体上限、限流）→ 再按契约校验请求、对要登录的接口验令牌 → 再做事；网页版的跨域在限流外面一层。
// 每个请求先解析成账号，接口只用这个账号的「人」取数据（隔离只在这一处做，见 internal/accounts）。
package appserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	middleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
)

// PathPrefix 是 App 接口的路径前缀（和契约里的路径一致）：公网只放行这一段。终端的语音连接、声纹登记在 /v1/ 下，
// 只在局域网 —— 两段 ⛔ 不能重叠，否则开放 App 接口时会把它们一起开出去。
const PathPrefix = "/api/v1/"

// maxBodyBytes 是请求体的上限（这一批的接口都是小的 JSON；以后传图片的接口另定）。
const maxBodyBytes = 64 << 10

// Accounts 是接口要用的账号库（internal/accounts.Store 实现）。
type Accounts interface {
	Authenticate(ctx context.Context, accessToken string) (accounts.Account, error)
	Refresh(ctx context.Context, refreshToken string) (accounts.TokenPair, error)
	Revoke(ctx context.Context, refreshToken string) error
	IssueTokens(ctx context.Context, accountID string) (accounts.TokenPair, error)
	ResolveLogin(ctx context.Context, subject, displayName, invite string) (accounts.Account, error)
	ByID(ctx context.Context, id string) (accounts.Account, error)
	List(ctx context.Context) ([]accounts.Account, error)
	Touch(ctx context.Context, id, appVersion string) error
	UpdateProfile(ctx context.Context, id, displayName, currency string) (accounts.Account, error)
}

// Spending 是花费的汇总（spending.Guard 实现）。
type Spending interface {
	// SummaryFor 是一个账号看到的汇总（account 为空 = 管理员：全局的）。
	SummaryFor(ctx context.Context, account string) (spending.Summary, error)
	// AccountSpent、AccountCap：一个账号自己这个月花了多少、管理员给它定的上限（管理员的账号列表用）。
	AccountSpent(ctx context.Context, account string) (spending.Spent, error)
	AccountCap(ctx context.Context, account string) (money.Amount, error)
}

// Memory 是某个人记着的事（memory.Keeper 实现）：有几条、列出、忘掉、找回。
type Memory interface {
	Counts(ctx context.Context, person string) (resident, notes int, err error)
	List(ctx context.Context, person string, withForgotten bool) (memory.Listing, error)
	Forget(ctx context.Context, person string, id int64) (notes.Note, error)
	Restore(ctx context.Context, person string, id int64) (notes.Note, error)
}

// Terminals 是现在在线的终端（brainserver.OnlineTerminals 实现）。
type Terminals interface {
	Unidentified() (online, needsUpgrade int)               // 没带设备凭证、连着的（过渡期：都归管理员账号）
	Status(terminalID string) (online bool, version string) // 这台现在连没连着、连着时报的版本
	Disconnect(terminalID string)                           // 断开这台正连着的会话（移除之后）
}

// TerminalRegistry 是终端的登记和归属（accounts.Store 实现）。
type TerminalRegistry interface {
	TerminalsOf(ctx context.Context, accountID string) ([]accounts.Terminal, error)
	PairRegistration(ctx context.Context, accountID, code string) (accounts.Terminal, error)
	RenameTerminal(ctx context.Context, accountID, terminalID, name string) (accounts.Terminal, error)
	RemoveTerminal(ctx context.Context, accountID, terminalID string) error
	PendingRegistrations(ctx context.Context) ([]accounts.PendingRegistration, error)
	ApproveRegistration(ctx context.Context, registrationID string) (accounts.Terminal, error)
	RejectRegistration(ctx context.Context, registrationID string) error
}

// Research 是某个人的后台调查（research.Runner 实现）：进行中的有几个、最近的、一个。
type Research interface {
	Running(ctx context.Context, person string) (int, error)
	Tasks(ctx context.Context, person string, limit int) ([]research.Task, error)
	Task(ctx context.Context, person string, id int64) (research.Task, error)
}

// Backstory 是背景记录池（backstory.Service 实现）。
type Backstory interface {
	View(ctx context.Context, person string) (conversationstore.Backstory, int, error)
	Edit(ctx context.Context, person, id, text string) error
	Delete(ctx context.Context, person, id string) error
	SetEnabled(ctx context.Context, person string, enabled bool) error
}

// Options 是接口服务的依赖和设置。Memory、Research 可以是 nil（没开那组工具）。
type Options struct {
	Accounts        Accounts
	Spending        Spending
	Memory          Memory
	Research        Research
	Terminals       Terminals        // 现在连着的终端；nil = 不知道（都当成不在线）
	Registry        TerminalRegistry // 终端的登记和归属；nil = 没有（终端的接口回 501）
	Chat            Chat             // 打字对话；nil = 没有（对话的接口回 501）
	Backstory       Backstory        // 背景记录池；nil = 没有（那几个接口回 501）
	Calendar        Calendar         // 日历；nil = 没有（日历的接口回 501）
	Expenses        Expenses         // 记账；nil = 没有（记账的接口回 501）
	Admin           AccountAdmin     // 管理员管账号、邀请码；nil = 没有（那几个接口回 501）
	Rates           money.Converter  // 汇总记账时换成默认货币用的汇率；nil = 只认同一种币种
	DefaultCurrency string           // 服务端的默认货币（账号没改时用）
	MinAppVersion   string           // 支持的最低 App 版本
	CapOptions      []float64        // 管理员定月上限的可选档位（默认货币）
	ProfileMaxChars int              // 主人资料最多多少字
	Login           *LoginOptions    // nil = 不开登录（测试）
	Household       Household        // 每个账号的主人资料和家；nil = 没有（那几个接口回 501）
	Places          PlaceFinder      // 搜地名；nil = 没有（天气的工具组关着）
	WebURL          string           // 网页版的公开根地址：放行它跨域调接口、登录完回它的回调页；空 = 没有网页版
	// Phrases：每种语言的固定的话（工具给人看的说明在这里，和大脑同一份）；零值 = 程序里带的那份
	Phrases brain.Phrases
	Logger  *slog.Logger
}

// Server 实现 appapi.StrictServerInterface。
type Server struct {
	opts     Options
	accounts Accounts
	texts    *texts
	spec     *openapi3.T
	logger   *slog.Logger
	pairing  pairFailures
}

var _ appapi.StrictServerInterface = (*Server)(nil)

// NewHandler 建 App 接口的 http.Handler（挂在 PathPrefix 下）。
func NewHandler(ctx context.Context, opts Options) (http.Handler, error) {
	if opts.Phrases.Languages == nil {
		phrases, err := brain.LoadPhrases("")
		if err != nil {
			return nil, err
		}
		opts.Phrases = phrases
	}
	loadedTexts, err := loadTexts()
	if err != nil {
		return nil, err
	}
	spec, err := appapi.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("读契约: %w", err)
	}
	spec.Servers = nil // 按路径匹配，不按契约里占位的地址
	server := &Server{opts: opts, accounts: opts.Accounts, texts: loadedTexts, spec: spec, logger: opts.Logger,
		pairing: pairFailures{failures: map[string][]time.Time{}}}

	mux := http.NewServeMux()
	limiter := newRateLimiter(ctx)
	if opts.Login != nil {
		login, err := server.newLogin(ctx, *opts.Login)
		if err != nil {
			return nil, err
		}
		mux.Handle("GET "+startPath, limiter.wrap(http.HandlerFunc(login.start), server))
		mux.Handle("GET "+callbackPath, limiter.wrap(http.HandlerFunc(login.callback), server))
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
	mux.Handle(sendMessagePattern, limiter.wrap(http.HandlerFunc(server.sendMessage), server)) // 回答是流：不经生成的代码
	mux.Handle(decidePattern, limiter.wrap(http.HandlerFunc(server.decideConfirmation), server))
	mux.Handle(uploadPattern, limiter.wrap(http.HandlerFunc(server.uploadAttachment), server)) // 请求体是图片：不经生成的代码
	mux.Handle(attachmentPattern, limiter.wrap(http.HandlerFunc(server.getAttachment), server))
	mux.Handle(receiptUploadPattern, limiter.wrap(http.HandlerFunc(server.uploadReceipt), server))
	mux.Handle(receiptImagePattern, limiter.wrap(http.HandlerFunc(server.getReceiptImage), server))
	mux.Handle(expenseImagePattern, limiter.wrap(http.HandlerFunc(server.getExpenseImage), server))
	mux.Handle(PathPrefix, limiter.wrap(validator(generated), server))
	// 跨域套在限流外面：被限流、出错的回答也带上跨域的头，网页版才读得到 429 / problem（预检由 cors 直接回）
	withCrossOrigin, err := crossOrigin(spec, opts.WebURL, mux)
	if err != nil {
		return nil, err
	}
	return server.outermost(withCrossOrigin), nil
}

// largeBodies 是请求体上限比 maxBodyBytes 大的路由（按路由匹配，不比路径的字符串）。
var largeBodies = func() *http.ServeMux {
	routes := http.NewServeMux()
	routes.Handle(uploadPattern, http.NotFoundHandler())
	routes.Handle(receiptUploadPattern, http.NotFoundHandler())
	routes.Handle(syncPattern, http.NotFoundHandler())
	return routes
}()

// bodyLimit 是这个请求的请求体上限：传图片、交小票的 attachmentMaxBytes，同步系统日历的 syncMaxBytes，其余 maxBodyBytes。
func bodyLimit(r *http.Request) int64 {
	switch _, pattern := largeBodies.Handler(r); pattern {
	case uploadPattern, receiptUploadPattern:
		return attachmentMaxBytes
	case syncPattern:
		return syncMaxBytes
	}
	return maxBodyBytes
}

// outermost 是最外层：请求体上限、放一个空的 caller。
func (s *Server) outermost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r))
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
