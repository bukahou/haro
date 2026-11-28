package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// fakeServer：登记后第一次问是等着、第二次是 outcome。
func fakeServer(t *testing.T, outcome realtimeproto.RegistrationOutcome) *httptest.Server {
	t.Helper()
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc(realtimeproto.RegistrationRoute, func(w http.ResponseWriter, r *http.Request) {
		var request realtimeproto.RegistrationRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Name != "客厅" {
			t.Errorf("报的名字：%q", request.Name)
		}
		_ = json.NewEncoder(w).Encode(realtimeproto.RegistrationStarted{Code: "ABCD-EFGH", ShortID: "A1B2C3", PollToken: "p", ExpiresIn: 600, Interval: 1})
	})
	mux.HandleFunc(realtimeproto.RegistrationPollRoute, func(w http.ResponseWriter, _ *http.Request) {
		polls++
		if polls == 1 {
			_ = json.NewEncoder(w).Encode(realtimeproto.RegistrationOutcome{Status: realtimeproto.RegistrationPending})
			return
		}
		_ = json.NewEncoder(w).Encode(outcome)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// 批准了：设备凭证存进文件（只有自己能读），读得回来；配对码、短编号显示给人看。
func TestPairApproved(t *testing.T) {
	server := fakeServer(t, realtimeproto.RegistrationOutcome{Status: realtimeproto.RegistrationApproved, TerminalID: "t1", Credential: "secret"})
	path := filepath.Join(t.TempDir(), "haro", "device-credential")
	var out bytes.Buffer
	if err := Run(context.Background(), Options{ServerURL: server.URL, Name: "客厅", CredentialFile: path, Out: &out, Client: server.Client()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ABCD-EFGH") || !strings.Contains(out.String(), "A1B2C3") {
		t.Errorf("显示的：%s", out.String())
	}
	if credential, err := LoadCredential(path); err != nil || credential != "secret" {
		t.Errorf("存下的凭证：%q %v", credential, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("凭证文件的权限：%v %v", info.Mode(), err)
	}
}

// 被拒绝：报 ErrRejected，不存文件；没有文件时 LoadCredential 是空、不出错。
func TestPairRejected(t *testing.T) {
	server := fakeServer(t, realtimeproto.RegistrationOutcome{Status: realtimeproto.RegistrationRejected})
	path := filepath.Join(t.TempDir(), "device-credential")
	err := Run(context.Background(), Options{ServerURL: server.URL, Name: "客厅", CredentialFile: path, Out: &bytes.Buffer{}, Client: server.Client()})
	if !errors.Is(err, ErrRejected) {
		t.Errorf("被拒绝：%v", err)
	}
	if credential, err := LoadCredential(path); err != nil || credential != "" {
		t.Errorf("没配对：%q %v", credential, err)
	}
}
