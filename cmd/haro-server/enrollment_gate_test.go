package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// gateTerminals 认两个凭证：admin（管理员的终端）、member（别的账号 a1 的终端）。
type gateTerminals struct{}

func (gateTerminals) Authenticate(_ context.Context, credential, _, _ string) (brainserver.Identity, error) {
	switch credential {
	case "admin":
		return brainserver.Identity{TerminalID: "t0", AccountID: "a0", Owner: "主人", Admin: true}, nil
	case "member":
		return brainserver.Identity{TerminalID: "t1", AccountID: "a1", Owner: "account:a1", Guest: "account:a1:guest"}, nil
	}
	return brainserver.Identity{}, brainserver.ErrInvalidCredential
}

// 声纹登记的门：管理员的终端照旧用填的名字；别的账号的终端登记成它的本人（account-a1），回给它的「认得哪些人」只有它自己；
// 局域网没带凭证照旧；公网没带凭证、凭证不对的 401；haro-asr 收不到凭证。
func TestEnrollmentGate(t *testing.T) {
	var gotPath, gotAuthorization string
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuthorization = r.URL.Path, r.Header.Get("Authorization")
		name := strings.TrimPrefix(r.URL.Path, realtimeproto.VoiceprintPath)
		_ = json.NewEncoder(w).Encode(realtimeproto.VoiceprintEnrollmentResult{Name: name, Sentences: 3, Speakers: []string{"主人", name, "account-a2"}})
	}))
	defer asr.Close()
	target, _ := url.Parse(asr.URL)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	serve := func(public bool, name, credential string) (int, realtimeproto.VoiceprintEnrollmentResult) {
		t.Helper()
		mux := http.NewServeMux()
		mux.Handle(realtimeproto.VoiceprintRoute, enrollmentGate{asr: httputil.NewSingleHostReverseProxy(target), terminals: gateTerminals{},
			requireCredential: public, logger: quiet})
		request := httptest.NewRequest(http.MethodPost, realtimeproto.VoiceprintPath+url.PathEscape(name), strings.NewReader(`{}`))
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		var result realtimeproto.VoiceprintEnrollmentResult
		_ = json.Unmarshal(recorder.Body.Bytes(), &result)
		return recorder.Code, result
	}
	if status, result := serve(true, "妈妈", "admin"); status != 200 || gotPath != realtimeproto.VoiceprintPath+"妈妈" || len(result.Speakers) != 3 {
		t.Errorf("管理员的终端：%d %s %+v", status, gotPath, result)
	}
	if gotAuthorization != "" {
		t.Error("haro-asr 不该收到凭证")
	}
	status, result := serve(true, "主人", "member")
	if status != 200 || gotPath != realtimeproto.VoiceprintPath+"account-a1" || len(result.Speakers) != 1 || result.Speakers[0] != "account-a1" {
		t.Errorf("别的账号的终端：%d %s %+v", status, gotPath, result)
	}
	if status, _ := serve(false, "主人", ""); status != 200 || gotPath != realtimeproto.VoiceprintPath+"主人" {
		t.Errorf("局域网没带凭证：%d %s", status, gotPath)
	}
	for name, credential := range map[string]string{"公网没带凭证": "", "凭证不对": "bad"} {
		gotPath = ""
		if status, _ := serve(true, "主人", credential); status != http.StatusUnauthorized || gotPath != "" {
			t.Errorf("%s：%d（转给了 %q）", name, status, gotPath)
		}
	}
}
