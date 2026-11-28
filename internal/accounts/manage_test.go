package accounts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 邀请码：带着有效的码登录 = 正常账号（来源 = 邀请），码用过了；再用、撤销了的、过期的、写错的都不行且不建账号；
// 见过的人带码不用掉；码只存 SHA-256（手输的小写、不带「-」也认）。
func TestInvites(t *testing.T) {
	store := openTest(t)
	ctx := context.Background()
	admin, _ := store.EnsureAdmin(ctx, testAdmin)
	now := time.Now()
	store.now = func() time.Time { return now }
	invite, code, err := store.CreateInvite(ctx, admin.ID, "给妈妈", 48*time.Hour)
	if err != nil || len(code) != 14 || invite.Status(now) != InviteUnused {
		t.Fatalf("生成：%+v %q %v", invite, code, err)
	}
	typed := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	joined, err := store.ResolveLogin(ctx, "sub-mom", "妈妈", typed)
	if err != nil || joined.Status != StatusActive || joined.JoinedVia != JoinedViaInvite {
		t.Fatalf("带码登录：%+v %v", joined, err)
	}
	if again, err := store.ResolveLogin(ctx, "sub-mom", "妈妈", code); err != nil || again.ID != joined.ID {
		t.Errorf("见过的人带码：%+v %v", again, err)
	}
	if _, err := store.ResolveLogin(ctx, "sub-other", "别人", code); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("用过的码：%v", err)
	}
	if _, err := store.BySubject(ctx, "sub-other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("无效的码不建账号：%v", err)
	}
	revoked, revokedCode, _ := store.CreateInvite(ctx, admin.ID, "", time.Hour)
	if err := store.RevokeInvite(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeInvite(ctx, revoked.ID); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("撤销两次：%v", err)
	}
	if _, err := store.ResolveLogin(ctx, "sub-other", "别人", revokedCode); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("撤销了的码：%v", err)
	}
	_, expiring, _ := store.CreateInvite(ctx, admin.ID, "", time.Hour)
	store.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := store.ResolveLogin(ctx, "sub-other", "别人", expiring); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("过期的码：%v", err)
	}
	if _, _, err := store.CreateInvite(ctx, admin.ID, "", MaxInviteTTL+time.Hour); err == nil {
		t.Error("有效期太长应报错")
	}
	invites, _ := store.Invites(ctx)
	statuses := map[string]int{}
	for _, each := range invites {
		statuses[each.Status(store.now())]++
		if each.Status(store.now()) == InviteUsed && each.UsedBy != joined.ID {
			t.Errorf("用的人：%+v", each)
		}
	}
	if statuses[InviteUsed] != 1 || statuses[InviteRevoked] != 1 || statuses[InviteExpired] != 1 {
		t.Errorf("状态：%v", statuses)
	}
}

// 批准、停用（令牌作废）、恢复、软删除（到期时间）、撤销删除；管理员停用、删除不了；状态不对的报 ErrAccountState。
func TestManageAccounts(t *testing.T) {
	store := openTest(t)
	ctx := context.Background()
	admin, _ := store.EnsureAdmin(ctx, testAdmin)
	if _, err := store.ResolveLogin(ctx, "sub-guest", "客人", ""); !errors.Is(err, ErrPendingApproval) {
		t.Fatal(err)
	}
	guest, _ := store.BySubject(ctx, "sub-guest")
	if _, err := store.Enable(ctx, guest.ID); !errors.Is(err, ErrAccountState) {
		t.Errorf("在等的不能恢复：%v", err)
	}
	approved, err := store.Approve(ctx, guest.ID)
	if err != nil || approved.Status != StatusActive {
		t.Fatalf("批准：%+v %v", approved, err)
	}
	if _, err := store.Approve(ctx, guest.ID); !errors.Is(err, ErrAccountState) {
		t.Errorf("批准两次：%v", err)
	}
	pair, _ := store.IssueTokens(ctx, guest.ID)
	if _, err := store.Disable(ctx, guest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, pair.AccessToken); err == nil {
		t.Error("停用后令牌应作废")
	}
	if _, err := store.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Error("停用后刷新令牌应作废")
	}
	if enabled, err := store.Enable(ctx, guest.ID); err != nil || enabled.Status != StatusActive {
		t.Errorf("恢复：%+v %v", enabled, err)
	}
	now := time.Now()
	store.now = func() time.Time { return now }
	deleted, err := store.ScheduleDeletion(ctx, guest.ID)
	if err != nil || deleted.Status != StatusDisabled || !deleted.DeletionDueAt.Equal(now.Add(DeletionGrace).Truncate(time.Second).UTC()) {
		t.Fatalf("软删除：%+v %v", deleted, err)
	}
	store.now = func() time.Time { return now.Add(time.Hour) }
	if again, _ := store.ScheduleDeletion(ctx, guest.ID); !again.DeletionDueAt.Equal(deleted.DeletionDueAt) {
		t.Errorf("再删一次不改到期时间：%v", again.DeletionDueAt)
	}
	if restored, err := store.Enable(ctx, guest.ID); err != nil || !restored.DeletionDueAt.IsZero() || restored.Status != StatusActive {
		t.Errorf("撤销删除：%+v %v", restored, err)
	}
	for name, change := range map[string]func(context.Context, string) (Account, error){"停用": store.Disable, "删除": store.ScheduleDeletion} {
		if _, err := change(ctx, admin.ID); !errors.Is(err, ErrIsAdmin) {
			t.Errorf("%s管理员：%v", name, err)
		}
	}
	if _, err := store.Approve(ctx, "没有这个"); !errors.Is(err, ErrNotFound) {
		t.Errorf("没有的账号：%v", err)
	}
}

// fakeData 记下删了谁；failing 时报错。
type fakeData struct {
	erased  []string
	failing bool
}

func (f *fakeData) ErasePerson(_ context.Context, person string) error {
	if f.failing {
		return errors.New("盘坏了")
	}
	f.erased = append(f.erased, person)
	return nil
}

// 软删除到期才彻底删：每一处删掉本人、访客两个人，全部成了才删账号；有一处出错就留着，下次再试。
func TestPurgeDue(t *testing.T) {
	store := openTest(t)
	ctx := context.Background()
	_, _ = store.EnsureAdmin(ctx, testAdmin)
	_, _ = store.ResolveLogin(ctx, "sub-guest", "客人", "")
	guest, _ := store.BySubject(ctx, "sub-guest")
	now := time.Now()
	store.now = func() time.Time { return now }
	if _, err := store.ScheduleDeletion(ctx, guest.ID); err != nil {
		t.Fatal(err)
	}
	good, bad := &fakeData{}, &fakeData{failing: true}
	purger := Purger{Store: store, Data: []NamedData{{Name: "好的", Data: good}, {Name: "坏的", Data: bad}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := purger.PurgeDue(ctx); err != nil || len(good.erased) != 0 {
		t.Errorf("没到期：%v %v", err, good.erased)
	}
	store.now = func() time.Time { return now.Add(DeletionGrace + time.Minute) }
	if err := purger.PurgeDue(ctx); err == nil {
		t.Error("有一处出错应报错")
	}
	if _, err := store.ByID(ctx, guest.ID); err != nil {
		t.Errorf("出错时账号留着：%v", err)
	}
	bad.failing = false
	good.erased = nil
	if err := purger.PurgeDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(good.erased) != 2 || good.erased[0] != guest.OwnerPerson || good.erased[1] != guest.GuestPerson || len(bad.erased) != 2 {
		t.Errorf("删了谁：%v %v", good.erased, bad.erased)
	}
	if _, err := store.ByID(ctx, guest.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("账号应删掉：%v", err)
	}
}
