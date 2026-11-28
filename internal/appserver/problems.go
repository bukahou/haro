package appserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
)

// problemType 是问题类型的 URI（RFC 9457）：haro 不另开文档页，一律 about:blank，App 按 code 判断。
const problemType = "about:blank"

// problem 是一个出错的回答：HTTP 状态、稳定的错误码、给人看的说明（按语言）。
func (s *Server) problem(r *http.Request, status int, code appapi.ErrorCode, values map[string]string) appapi.Problem {
	detail := s.texts.say(s.texts.pick(r.Header.Get("Accept-Language")), "problem."+string(code), values)
	return appapi.Problem{Type: problemType, Title: http.StatusText(status), Status: status, Code: code, Detail: &detail}
}

// writeProblem 直接写出一个出错的回答（中间件、手写挂载的接口用；生成的接口用各自的响应类型）。
func (s *Server) writeProblem(w http.ResponseWriter, r *http.Request, status int, code appapi.ErrorCode, values map[string]string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s.problem(r, status, code, values))
}

// accountProblem 是账号、令牌的错误对应的状态和错误码（按错误的类型分，⛔ 不看文字）；认不出的是 500。
func accountProblem(err error) (int, appapi.ErrorCode) {
	switch {
	case errors.Is(err, accounts.ErrTokenExpired):
		return http.StatusUnauthorized, appapi.ErrorCodeTokenExpired
	case errors.Is(err, accounts.ErrInvalidToken):
		return http.StatusUnauthorized, appapi.ErrorCodeInvalidToken
	case errors.Is(err, accounts.ErrPendingApproval):
		return http.StatusUnauthorized, appapi.ErrorCodePendingApproval
	case errors.Is(err, accounts.ErrDisabled):
		return http.StatusUnauthorized, appapi.ErrorCodeAccountDisabled
	case errors.Is(err, accounts.ErrNotFound):
		return http.StatusNotFound, appapi.ErrorCodeNotFound
	}
	return http.StatusInternalServerError, appapi.ErrorCodeInternal
}
