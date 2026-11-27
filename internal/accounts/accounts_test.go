package accounts

import (
	"context"
	"errors"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

var testAdmin = Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: ""}

// 管理员由配置定：没有就建（人用原来的声纹名和全家共用），再启动时不重复建；配置换了人，以前的管理员变回普通账号。
func TestEnsureAdmin(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	admin, err := store.EnsureAdmin(ctx, testAdmin)
	if err != nil || admin.Role != RoleAdmin || admin.Status != StatusActive || admin.OwnerPerson != "主人" || admin.GuestPerson != "" {
		t.Fatalf("管理员账号：%+v %v", admin, err)
	}
	again, err := store.EnsureAdmin(ctx, testAdmin)
	if err != nil || again.ID != admin.ID {
		t.Errorf("再启动时用同一个账号：%+v %v", again, err)
	}
	if _, err := store.EnsureAdmin(ctx, Admin{Subject: "sub-owner", DisplayName: "主人", Person: "别人", GuestPerson: ""}); err == nil {
		t.Error("管理员原来对应的人改了：数据会对不上，应报错")
	}
	if _, err := store.EnsureAdmin(ctx, Admin{Subject: "sub-new", DisplayName: "新管理员", Person: "新", GuestPerson: "新的访客"}); err != nil {
		t.Fatal(err)
	}
	if old, _ := store.ByID(ctx, admin.ID); old.Role != RoleMember {
		t.Errorf("配置换了管理员，以前的应变回普通账号：%+v", old)
	}
}

// 第一次登录的 sub：建一个等管理员同意的账号；它的人用账号编号生成，不和别人重名。停用的账号登录报 ErrDisabled。
func TestResolveLogin(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	if _, err := store.EnsureAdmin(ctx, testAdmin); err != nil {
		t.Fatal(err)
	}
	if account, err := store.ResolveLogin(ctx, "sub-owner", ""); err != nil || account.Role != RoleAdmin {
		t.Errorf("管理员登录：%+v %v", account, err)
	}
	if _, err := store.ResolveLogin(ctx, "sub-guest", "客人"); !errors.Is(err, ErrPendingApproval) {
		t.Fatalf("新的 sub 应等同意：%v", err)
	}
	pending, err := store.BySubject(ctx, "sub-guest")
	if err != nil || pending.Status != StatusPendingApproval || pending.OwnerPerson == "主人" || pending.OwnerPerson == pending.GuestPerson {
		t.Errorf("等同意的账号：%+v %v", pending, err)
	}
	if _, err := store.ResolveLogin(ctx, "sub-guest", "客人"); !errors.Is(err, ErrPendingApproval) {
		t.Errorf("再登录还是等同意：%v", err)
	}
	if _, err := store.db.Exec(`UPDATE accounts SET status = ? WHERE id = ?`, StatusDisabled, pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveLogin(ctx, "sub-guest", "客人"); !errors.Is(err, ErrDisabled) {
		t.Errorf("停用的账号：%v", err)
	}
}

// 令牌：访问令牌能找到账号，过期了报 ErrTokenExpired；刷新令牌用一次就换新；
// 用过的刷新令牌又被拿来用（被盗）：整串作废，连新发的访问令牌也不能用；登出作废整串。
func TestTokens(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	admin, err := store.EnsureAdmin(ctx, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	first, err := store.IssueTokens(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account, err := store.Authenticate(ctx, first.AccessToken); err != nil || account.ID != admin.ID {
		t.Fatalf("访问令牌：%+v %v", account, err)
	}
	if _, err := store.Authenticate(ctx, "not-a-token"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("认不出的令牌：%v", err)
	}
	now = now.Add(AccessTokenTTL)
	if _, err := store.Authenticate(ctx, first.AccessToken); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("过期的访问令牌：%v", err)
	}
	second, err := store.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("刷新：%v", err)
	}
	if _, err := store.Authenticate(ctx, second.AccessToken); err != nil {
		t.Errorf("刷新后的访问令牌：%v", err)
	}
	if _, err := store.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("用过的刷新令牌又来用：应拒绝：%v", err)
	}
	if _, err := store.Authenticate(ctx, second.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("被盗用后整串作废，新的访问令牌也不能用：%v", err)
	}
	third, err := store.IssueTokens(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, third.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Refresh(ctx, third.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("登出后刷新令牌作废：%v", err)
	}
	if _, err := store.Authenticate(ctx, third.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("登出后访问令牌作废：%v", err)
	}
	if err := store.Revoke(ctx, "unknown"); err != nil {
		t.Errorf("本来就无效的令牌登出不报错：%v", err)
	}
	now = now.Add(RefreshTokenTTL)
	if _, err := store.Refresh(ctx, third.RefreshToken); err == nil {
		t.Error("过期的刷新令牌不能用")
	}
}

// 等同意、停用的账号拿到的令牌（例如之前发的）也不能用。
func TestTokensOfUnusableAccounts(t *testing.T) {
	ctx := context.Background()
	store := openTest(t)
	if _, err := store.ResolveLogin(ctx, "sub-guest", "客人"); !errors.Is(err, ErrPendingApproval) {
		t.Fatal(err)
	}
	pending, _ := store.BySubject(ctx, "sub-guest")
	pair, err := store.IssueTokens(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, pair.AccessToken); !errors.Is(err, ErrPendingApproval) {
		t.Errorf("等同意的账号：%v", err)
	}
	if _, err := store.Refresh(ctx, pair.RefreshToken); !errors.Is(err, ErrPendingApproval) {
		t.Errorf("等同意的账号不能刷新：%v", err)
	}
}
