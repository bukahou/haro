package brainserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Registrar 是终端登记的执行方（账号库经适配器实现）。轮询凭据不认得时返回 ErrRegistrationNotFound。
type Registrar interface {
	// StartRegistration 开始登记；invite 不为空时用邀请码直接批准（码不对报 ErrInviteInvalid）。
	StartRegistration(ctx context.Context, name, version, remote, invite string) (realtimeproto.RegistrationStarted, error)
	PollRegistration(ctx context.Context, pollToken string) (realtimeproto.RegistrationOutcome, error)
}

// ErrRegistrationNotFound 是轮询凭据不认得。
var ErrRegistrationNotFound = errors.New("没有这个登记")

// ErrInviteInvalid 是登记带的邀请码不对、过期了、用过了或者撤销了（Registrar 交回）。
var ErrInviteInvalid = errors.New("邀请码不对")

// 登记接口的限量：请求体很小；开始登记、问结果各一个令牌桶（整个服务端共用：现在只在局域网，开到公网时按来源分开）。
const (
	registrationBodyBytes   = 4 << 10
	registrationStartPerMin = 10
	registrationPollPerMin  = 120 // 每台终端每 5 秒问一次 ⇒ 够十台同时登记
)

// RegisterRegistrationRoutes 把终端登记的两个接口挂到 mux 上。
func RegisterRegistrationRoutes(mux *http.ServeMux, registrar Registrar, logger *slog.Logger) {
	startLimit := rate.NewLimiter(rate.Limit(float64(registrationStartPerMin)/60), registrationStartPerMin/2)
	pollLimit := rate.NewLimiter(rate.Limit(float64(registrationPollPerMin)/60), registrationPollPerMin/2)
	mux.HandleFunc(realtimeproto.RegistrationRoute, func(w http.ResponseWriter, r *http.Request) {
		var request realtimeproto.RegistrationRequest
		if !admitRegistration(w, r, startLimit, &request) {
			return
		}
		name := strings.TrimSpace(request.Name)
		if utf8.RuneCountInString(name) > realtimeproto.MaxTerminalNameRunes {
			name = string([]rune(name)[:realtimeproto.MaxTerminalNameRunes])
		}
		if name == "" {
			writeHTTPError(w, http.StatusBadRequest, realtimeproto.NewHTTPError(http.StatusBadRequest, realtimeproto.CodeInvalidEvent,
				"登记要带终端的名字"))
			return
		}
		started, err := registrar.StartRegistration(r.Context(), name, r.Header.Get(buildinfo.Header), remoteHost(r),
			strings.TrimSpace(request.Invite))
		if errors.Is(err, ErrInviteInvalid) {
			logger.Warn("终端登记带的邀请码不对", "remote", r.RemoteAddr, "name", name)
			writeHTTPError(w, http.StatusBadRequest, realtimeproto.NewHTTPError(http.StatusBadRequest, realtimeproto.CodeInviteInvalid,
				"邀请码不对、过期了或者用过了：向管理员要一个新的，或者不带邀请码登记（等管理员批准）"))
			return
		}
		if err != nil {
			logger.Error("终端登记出错", "remote", r.RemoteAddr, "err", err)
			writeHTTPError(w, http.StatusInternalServerError, realtimeproto.NewHTTPError(http.StatusInternalServerError,
				realtimeproto.CodeServerError, "服务端登记出错：稍后再试"))
			return
		}
		logger.Info("终端开始登记", "remote", r.RemoteAddr, "name", name, "invite", request.Invite != "")
		writeJSON(w, started)
	})
	mux.HandleFunc(realtimeproto.RegistrationPollRoute, func(w http.ResponseWriter, r *http.Request) {
		var poll realtimeproto.RegistrationPoll
		if !admitRegistration(w, r, pollLimit, &poll) {
			return
		}
		outcome, err := registrar.PollRegistration(r.Context(), poll.PollToken)
		switch {
		case errors.Is(err, ErrRegistrationNotFound):
			writeHTTPError(w, http.StatusNotFound, realtimeproto.NewHTTPError(http.StatusNotFound, realtimeproto.CodeRegistrationNotFound,
				"没有这个登记（已经取走凭证、或太久没来问）：重新登记"))
			return
		case err != nil:
			logger.Error("问登记结果出错", "remote", r.RemoteAddr, "err", err)
			writeHTTPError(w, http.StatusInternalServerError, realtimeproto.NewHTTPError(http.StatusInternalServerError,
				realtimeproto.CodeServerError, "服务端出错：稍后再问"))
			return
		}
		if outcome.Status != realtimeproto.RegistrationPending {
			logger.Info("终端登记有了结果", "remote", r.RemoteAddr, "status", outcome.Status, "terminal", outcome.TerminalID)
		}
		writeJSON(w, outcome)
	})
}

// admitRegistration 先限量（请求体上限、令牌桶）再解请求体；不放行时已经回好了。
func admitRegistration(w http.ResponseWriter, r *http.Request, limiter *rate.Limiter, into any) bool {
	if !limiter.Allow() {
		writeHTTPError(w, http.StatusTooManyRequests, realtimeproto.NewHTTPError(http.StatusTooManyRequests, realtimeproto.CodeRateLimited,
			"请求太频繁：过一会儿再试"))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, registrationBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		writeHTTPError(w, http.StatusBadRequest, realtimeproto.NewHTTPError(http.StatusBadRequest, realtimeproto.CodeInvalidEvent,
			"请求体解不开："+err.Error()))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// remoteHost 是连过来的地址（不带端口；给管理员看是哪台机器）。
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
