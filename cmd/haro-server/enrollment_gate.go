package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// enrollmentGate 守着声纹登记（转给 haro-asr 之前）：执行方自己核对是谁在登记、能登记成谁。
//   - 带了设备凭证、是管理员的终端：照旧，名字由终端填；
//   - 带了设备凭证、是别的账号的终端：只能登记成这个账号的本人（名字由服务端定，终端填的不算）；
//     回给它的结果里「认得哪些人」只留它自己（别的账号的声纹名不外露）；
//   - 没带凭证：局域网的入口照旧（过渡期），公网的入口（requireCredential）拒绝；凭证不对的一律拒绝。
type enrollmentGate struct {
	asr               *httputil.ReverseProxy
	terminals         brainserver.Authenticator // nil = 没有账号库（带了凭证的一律拒绝）
	requireCredential bool
	logger            *slog.Logger
}

func (g enrollmentGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	credential, hasCredential := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	credential = strings.TrimSpace(credential)
	switch {
	case !hasCredential && g.requireCredential:
		g.refuse(w, r, "这个入口登记声纹要设备凭证：先配对这台终端（haro -pair）")
		return
	case !hasCredential:
		g.asr.ServeHTTP(w, r)
		return
	case credential == "" || g.terminals == nil:
		g.refuse(w, r, "设备凭证不对：重新配对这台终端")
		return
	}
	identity, err := g.terminals.Authenticate(r.Context(), credential, r.Header.Get(buildinfo.Header), r.RemoteAddr)
	switch {
	case errors.Is(err, brainserver.ErrInvalidCredential):
		g.refuse(w, r, "这台终端的设备凭证无效（可能已在 App 里移除）：重新配对")
		return
	case err != nil:
		g.logger.Error("登记声纹时验设备凭证出错", "remote", r.RemoteAddr, "err", err)
		writeEnrollmentError(w, http.StatusInternalServerError, realtimeproto.CodeServerError, "服务端验设备凭证出错：稍后再试")
		return
	}
	r.Header.Del("Authorization") // haro-asr 不看凭证
	if identity.Admin || r.Method != http.MethodPost {
		g.asr.ServeHTTP(w, r)
		return
	}
	own := accounts.VoiceprintNameOf(identity.Owner)
	forced := r.Clone(r.Context())
	forced.URL.Path, forced.URL.RawPath = realtimeproto.VoiceprintPath+own, ""
	g.logger.Info("别的账号的终端登记声纹：登记成这个账号的本人", "terminal", identity.TerminalID, "account", identity.AccountID)
	recorder := &capturedResponse{header: http.Header{}, status: http.StatusOK}
	g.asr.ServeHTTP(recorder, forced)
	body := recorder.body.Bytes()
	if recorder.status == http.StatusOK {
		var result realtimeproto.VoiceprintEnrollmentResult
		if err := json.Unmarshal(body, &result); err == nil {
			result.Speakers = []string{own}
			if filtered, err := json.Marshal(result); err == nil {
				body = filtered
			}
		}
	}
	for key, values := range recorder.header {
		w.Header()[key] = values
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(recorder.status)
	_, _ = w.Write(body)
}

func (g enrollmentGate) refuse(w http.ResponseWriter, r *http.Request, message string) {
	g.logger.Warn("拒绝登记声纹", "remote", r.RemoteAddr, "reason", message)
	writeEnrollmentError(w, http.StatusUnauthorized, realtimeproto.CodeInvalidCredential, message)
}

func writeEnrollmentError(w http.ResponseWriter, status int, code realtimeproto.ErrorCode, message string) {
	data, _ := json.Marshal(realtimeproto.NewHTTPError(status, code, message))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// capturedResponse 收下 haro-asr 的回答（改过「认得哪些人」再交给终端）；回答很小（几句话的结果）。
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header         { return c.header }
func (c *capturedResponse) WriteHeader(status int)      { c.status = status }
func (c *capturedResponse) Write(p []byte) (int, error) { return c.body.Write(p) }
