package accounts

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/money"
)

// 配对：终端登记 → App 里输入配对码（大小写、「-」随便）→ 终端轮询拿到凭证（只拿一次）→ 用凭证连进来是这个账号；移除后凭证作废。
func TestPairTerminal(t *testing.T) {
	store := openTest(t)
	ctx := t.Context()
	owner, err := store.EnsureAdmin(ctx, Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: "_shared"})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := store.StartRegistration(ctx, "客厅", "v1", "192.0.2.1", "")
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
	approved, _ := store.StartRegistration(ctx, "书房", "v1", "", "")
	rejected, _ := store.StartRegistration(ctx, "门口", "v1", "", "")
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
	late, _ := store.StartRegistration(ctx, "阳台", "v1", "", "")
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

// 终端带邀请码登记：直接建一个匿名账号（带默认的月上限）、终端归它，第一次问结果就拿到凭证，码用掉；
// 码不对时不建、登记也不留（管理员的「新终端」里看不到）。
func TestRegistrationWithInvite(t *testing.T) {
	store := openTest(t)
	ctx := t.Context()
	admin, _ := store.EnsureAdmin(ctx, testAdmin)
	store.SetAnonymousMonthlyCap(money.Of(500, "JPY"))
	_, code, err := store.CreateInvite(ctx, admin.ID, "给爸爸的终端", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := store.StartRegistration(ctx, "爸爸家", "v1", "", code)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.PollRegistration(ctx, registration.PollToken)
	if err != nil || result.Status != RegistrationApproved || result.Credential == "" {
		t.Fatalf("第一次问就批准了：%+v %v", result, err)
	}
	_, account, err := store.AuthenticateTerminal(ctx, result.Credential)
	if err != nil || account.JoinedVia != JoinedViaTerminal || account.MonthlyCap != money.Of(500, "JPY") {
		t.Errorf("匿名账号：%+v %v", account, err)
	}
	invites, _ := store.Invites(ctx)
	if len(invites) != 1 || invites[0].UsedBy != account.ID {
		t.Errorf("码用掉了：%+v", invites)
	}
	if _, err := store.StartRegistration(ctx, "别人家", "v1", "", code); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("用过的码：%v", err)
	}
	if pending, _ := store.PendingRegistrations(ctx); len(pending) != 0 {
		t.Errorf("码不对的登记不留：%+v", pending)
	}
}

// 匿名账号的终端都很久没连过：找得出来、软删除（Purger）；最近连过的、有 akasha 账号的不算。
func TestIdleAnonymous(t *testing.T) {
	store := openTest(t)
	ctx := t.Context()
	_, _ = store.EnsureAdmin(ctx, testAdmin)
	now := time.Now()
	store.now = func() time.Time { return now.Add(-200 * 24 * time.Hour) }
	idle, _ := store.StartRegistration(ctx, "旧的", "v1", "", "")
	idleTerminal, _ := store.ApproveRegistration(ctx, idle.ID)
	active, _ := store.StartRegistration(ctx, "常用的", "v1", "", "")
	activeTerminal, _ := store.ApproveRegistration(ctx, active.ID)
	_, _ = store.ResolveLogin(ctx, "sub-old", "老用户", "")
	store.now = func() time.Time { return now }
	if err := store.TouchTerminal(ctx, activeTerminal.ID, "v1", ""); err != nil {
		t.Fatal(err)
	}
	found, err := store.IdleAnonymous(ctx, now.Add(-90*24*time.Hour))
	if err != nil || len(found) != 1 || found[0].ID != idleTerminal.AccountID {
		t.Fatalf("久不连接的：%+v %v", found, err)
	}
	purger := Purger{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleAnonymousAfter: 90 * 24 * time.Hour}
	if err := purger.PurgeDue(ctx); err != nil {
		t.Fatal(err)
	}
	if account, _ := store.ByID(ctx, idleTerminal.AccountID); account.DeletionDueAt.IsZero() || account.Status != StatusDisabled {
		t.Errorf("应软删除：%+v", account)
	}
	if account, _ := store.ByID(ctx, activeTerminal.AccountID); !account.DeletionDueAt.IsZero() {
		t.Errorf("常用的不动：%+v", account)
	}
	if again, _ := store.IdleAnonymous(ctx, now.Add(-90*24*time.Hour)); len(again) != 0 {
		t.Errorf("软删除了的不再算：%+v", again)
	}
}
