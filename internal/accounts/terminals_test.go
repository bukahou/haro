package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 配对：终端登记 → App 里输入配对码（大小写、「-」随便）→ 终端轮询拿到凭证（只拿一次）→ 用凭证连进来是这个账号；移除后凭证作废。
func TestPairTerminal(t *testing.T) {
	store := openTest(t)
	ctx := t.Context()
	owner, err := store.EnsureAdmin(ctx, Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: "_shared"})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := store.StartRegistration(ctx, "客厅", "v1", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(registration.Code) != pairCodeLength+1 || strings.ContainsAny(registration.Code, "0O1IL") {
		t.Errorf("配对码：%q", registration.Code)
	}
	if result, err := store.PollRegistration(ctx, registration.PollToken); err != nil || result.Status != RegistrationPending {
		t.Fatalf("还没配对：%+v %v", result, err)
	}
	if _, err := store.PairRegistration(ctx, owner.ID, "AAAA-AAAA"); !errors.Is(err, ErrPairCodeInvalid) {
		t.Errorf("配对码不对：%v", err)
	}
	typed := strings.ToLower(strings.ReplaceAll(registration.Code, "-", " "))
	terminal, err := store.PairRegistration(ctx, owner.ID, typed)
	if err != nil || terminal.AccountID != owner.ID || terminal.Name != "客厅" {
		t.Fatalf("配对：%+v %v", terminal, err)
	}
	if _, err := store.PairRegistration(ctx, owner.ID, registration.Code); !errors.Is(err, ErrPairCodeUsed) {
		t.Errorf("配对码只能用一次：%v", err)
	}
	result, err := store.PollRegistration(ctx, registration.PollToken)
	if err != nil || result.Status != RegistrationApproved || result.Credential == "" || result.TerminalID != terminal.ID {
		t.Fatalf("取凭证：%+v %v", result, err)
	}
	if _, err := store.PollRegistration(ctx, registration.PollToken); !errors.Is(err, ErrRegistrationNotFound) {
		t.Errorf("凭证只交出一次：%v", err)
	}
	connected, account, err := store.AuthenticateTerminal(ctx, result.Credential)
	if err != nil || connected.ID != terminal.ID || account.ID != owner.ID || !connected.CredentialIssued {
		t.Fatalf("用凭证连进来：%+v %+v %v", connected, account, err)
	}
	if _, err := store.RenameTerminal(ctx, "别人", terminal.ID, "卧室"); !errors.Is(err, ErrTerminalNotFound) {
		t.Errorf("别的账号改不了：%v", err)
	}
	if renamed, err := store.RenameTerminal(ctx, owner.ID, terminal.ID, "卧室"); err != nil || renamed.Name != "卧室" {
		t.Errorf("改名：%+v %v", renamed, err)
	}
	if err := store.RemoveTerminal(ctx, owner.ID, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateTerminal(ctx, result.Credential); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("移除后凭证作废：%v", err)
	}
}

// 管理员批准 = 新的匿名账号；拒绝、过期的终端轮询时知道结果；过期的配对码不能用。
func TestApproveRejectExpire(t *testing.T) {
	store := openTest(t)
	ctx := t.Context()
	approved, _ := store.StartRegistration(ctx, "书房", "v1", "")
	rejected, _ := store.StartRegistration(ctx, "门口", "v1", "")
	if pending, err := store.PendingRegistrations(ctx); err != nil || len(pending) != 2 {
		t.Fatalf("等着的：%+v %v", pending, err)
	}
	terminal, err := store.ApproveRegistration(ctx, approved.ID)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.ByID(ctx, terminal.AccountID)
	if err != nil || account.JoinedVia != JoinedViaTerminal || account.Status != StatusActive || account.Subject != "" {
		t.Errorf("匿名账号：%+v %v", account, err)
	}
	if err := store.RejectRegistration(ctx, rejected.ID); err != nil {
		t.Fatal(err)
	}
	if result, err := store.PollRegistration(ctx, rejected.PollToken); err != nil || result.Status != RegistrationRejected || result.Credential != "" {
		t.Errorf("被拒绝：%+v %v", result, err)
	}
	late, _ := store.StartRegistration(ctx, "阳台", "v1", "")
	store.now = func() time.Time { return time.Now().Add(RegistrationTTL + time.Minute) }
	if _, err := store.PairRegistration(ctx, "x", late.Code); !errors.Is(err, ErrPairCodeExpired) {
		t.Errorf("过期的配对码：%v", err)
	}
	if result, err := store.PollRegistration(ctx, late.PollToken); err != nil || result.Status != RegistrationExpired {
		t.Errorf("过期：%+v %v", result, err)
	}
	if pending, _ := store.PendingRegistrations(ctx); len(pending) != 0 {
		t.Errorf("过期的不再列出：%+v", pending)
	}
}
