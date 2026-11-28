package brainserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// fakeAuthenticator 只认一个凭证。
type fakeAuthenticator struct{ remote string }

func (f *fakeAuthenticator) Authenticate(_ context.Context, credential, _, remote string) (brainserver.Identity, error) {
	f.remote = remote
	if credential != "good" {
		return brainserver.Identity{}, brainserver.ErrInvalidCredential
	}
	return brainserver.Identity{TerminalID: "t1", AccountID: "a1", Owner: "account:a1", Guest: "account:a1:guest"}, nil
}

// 握手：带了有效的设备凭证 → 会话知道是哪台终端；凭证无效 → 401 invalid_credential（⛔ 不当成没带）；没带 → 照旧放行；
// 移除后（Disconnect）正连着的会话马上断开。
func TestTerminalCredentialAtHandshake(t *testing.T) {
	online := brainserver.NewOnlineTerminals()
	identities := make(chan *brainserver.Identity, 4)
	handler := brainserver.NewHandler(func(terminal brainserver.Terminal) brainserver.Session {
		identities <- terminal.Identity
		return newFakeSession()
	}, brainserver.Options{Online: online, Terminals: &fakeAuthenticator{}}, quiet)
	server := httptest.NewServer(handler)
	defer server.Close()
	dial := func(credential string) (*websocket.Conn, *http.Response, error) {
		header := http.Header{}
		if credential != "" {
			header.Set("Authorization", "Bearer "+credential)
		}
		return websocket.Dial(context.Background(), wsURL(server), &websocket.DialOptions{Subprotocols: []string{realtimeproto.Protocol},
			HTTPHeader: header})
	}
	if _, response, err := dial("bad"); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无效的凭证应 401：%v", err)
	}
	conn, _, err := dial("good")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if identity := <-identities; identity == nil || identity.TerminalID != "t1" {
		t.Errorf("会话拿到的终端：%+v", identity)
	}
	waitFor(t, func() bool { on, _ := online.Status("t1"); return on })
	legacy, _, err := dial("")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.CloseNow()
	if identity := <-identities; identity != nil {
		t.Errorf("没带凭证：%+v", identity)
	}
	waitFor(t, func() bool { on, _ := online.Unidentified(); return on == 1 })
	online.Disconnect("t1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil || websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("移除后应断开：%v", err)
	}
	waitFor(t, func() bool { on, _ := online.Status("t1"); return !on })
}

// 公网的入口（RequireCredential）：没带凭证的 haro 终端、普通 Realtime 客户端都 401；带了有效凭证的照常。
// 一个账号同时连着的终端到了上限（MaxOnlinePerAccount）：再来的 429 too_many_sessions；断开一台后又能连。
func TestPublicEntryAndPerAccountLimit(t *testing.T) {
	online := brainserver.NewOnlineTerminals()
	handler := brainserver.NewHandler(func(brainserver.Terminal) brainserver.Session { return newFakeSession() },
		brainserver.Options{Online: online, Terminals: &fakeAuthenticator{}, RequireCredential: true, MaxOnlinePerAccount: 1}, quiet)
	server := httptest.NewServer(handler)
	defer server.Close()
	dial := func(credential string, protocols ...string) (*websocket.Conn, *http.Response, error) {
		header := http.Header{}
		if credential != "" {
			header.Set("Authorization", "Bearer "+credential)
		}
		return websocket.Dial(context.Background(), wsURL(server), &websocket.DialOptions{Subprotocols: protocols, HTTPHeader: header})
	}
	if _, response, err := dial("", realtimeproto.Protocol); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Errorf("没带凭证的终端：%v", err)
	}
	if _, response, err := dial(""); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Errorf("普通 Realtime 客户端：%v", err)
	}
	first, _, err := dial("good", realtimeproto.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return online.AccountCount("a1") == 1 })
	if _, response, err := dial("good", realtimeproto.Protocol); err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("同一个账号第二台：%v", err)
	}
	first.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return online.AccountCount("a1") == 0 })
	again, _, err := dial("good", realtimeproto.Protocol)
	if err != nil {
		t.Errorf("断开一台后：%v", err)
	} else {
		again.CloseNow()
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for range 100 {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等不到")
}

// fakeRegistrar 记下登记，问结果时第一次是 pending、之后是 approved。
type fakeRegistrar struct{ polls int }

func (f *fakeRegistrar) StartRegistration(_ context.Context, name, _, _, invite string) (realtimeproto.RegistrationStarted, error) {
	if invite == "bad" {
		return realtimeproto.RegistrationStarted{}, brainserver.ErrInviteInvalid
	}
	return realtimeproto.RegistrationStarted{Code: "ABCD-EFGH", ShortID: "A1B2C3", PollToken: "poll-" + name, ExpiresIn: 600, Interval: 5}, nil
}

func (f *fakeRegistrar) PollRegistration(_ context.Context, token string) (realtimeproto.RegistrationOutcome, error) {
	if token != "poll-客厅" {
		return realtimeproto.RegistrationOutcome{}, brainserver.ErrRegistrationNotFound
	}
	f.polls++
	if f.polls == 1 {
		return realtimeproto.RegistrationOutcome{Status: realtimeproto.RegistrationPending}, nil
	}
	return realtimeproto.RegistrationOutcome{Status: realtimeproto.RegistrationApproved, TerminalID: "t1", Credential: "secret"}, nil
}

// 登记接口：开始 → 问结果（等着 → 批准了带凭证）；轮询凭据不认得 404；没名字 400；问得太勤 429。
func TestRegistrationRoutes(t *testing.T) {
	mux := http.NewServeMux()
	brainserver.RegisterRegistrationRoutes(mux, &fakeRegistrar{}, quiet)
	server := httptest.NewServer(mux)
	defer server.Close()
	post := func(path string, body any, into any) int {
		data, _ := json.Marshal(body)
		response, err := http.Post(server.URL+path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if into != nil {
			_ = json.NewDecoder(response.Body).Decode(into)
		}
		return response.StatusCode
	}
	var started realtimeproto.RegistrationStarted
	if status := post(realtimeproto.RegistrationPath, realtimeproto.RegistrationRequest{Name: "客厅"}, &started); status != 200 ||
		started.Code != "ABCD-EFGH" || started.ShortID == "" {
		t.Fatalf("开始登记：%d %+v", status, started)
	}
	if status := post(realtimeproto.RegistrationPath, realtimeproto.RegistrationRequest{Name: " "}, nil); status != 400 {
		t.Errorf("没名字：%d", status)
	}
	var refused realtimeproto.HTTPError
	if status := post(realtimeproto.RegistrationPath, realtimeproto.RegistrationRequest{Name: "客厅", Invite: "bad"}, &refused); status != 400 ||
		refused.Error.Code != realtimeproto.CodeInviteInvalid {
		t.Errorf("邀请码不对：%d %+v", status, refused)
	}
	var outcome realtimeproto.RegistrationOutcome
	if status := post(realtimeproto.RegistrationPollPath, realtimeproto.RegistrationPoll{PollToken: started.PollToken}, &outcome); status != 200 ||
		outcome.Status != realtimeproto.RegistrationPending {
		t.Errorf("等着：%d %+v", status, outcome)
	}
	if status := post(realtimeproto.RegistrationPollPath, realtimeproto.RegistrationPoll{PollToken: started.PollToken}, &outcome); status != 200 ||
		outcome.Status != realtimeproto.RegistrationApproved || outcome.Credential != "secret" {
		t.Errorf("批准了：%d %+v", status, outcome)
	}
	var problem realtimeproto.HTTPError
	if status := post(realtimeproto.RegistrationPollPath, realtimeproto.RegistrationPoll{PollToken: "x"}, &problem); status != 404 ||
		problem.Error.Code != realtimeproto.CodeRegistrationNotFound {
		t.Errorf("不认得的轮询凭据：%d %+v", status, problem)
	}
	limited := false
	for range 20 {
		if post(realtimeproto.RegistrationPath, realtimeproto.RegistrationRequest{Name: "试"}, nil) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("一下子 20 次登记应被限流")
	}
}
