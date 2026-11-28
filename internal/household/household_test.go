package household

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/profile"
	"github.com/bukahou/haro/internal/weather"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// fixture：资料文件（全家 + 主人）、配置里的家（固定坐标）和节假日、一个管理员账号和一个普通账号。
type fixture struct {
	service *Service
	store   *accounts.Store
	admin   accounts.Account
	member  accounts.Account
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		profile.HouseholdFile: "家里有一只猫。<!-- 填写说明 -->",
		"owner.md":            "---\nperson: 主人\n---\n主人早上喝咖啡。",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := accounts.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, err := store.EnsureAdmin(t.Context(), accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: ""})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.ResolveLogin(t.Context(), "sub-member", "别人")
	if !errors.Is(err, accounts.ErrPendingApproval) {
		t.Fatalf("新账号应在等同意：%v", err)
	}
	if member, err = store.BySubject(t.Context(), "sub-member"); err != nil {
		t.Fatal(err)
	}
	defaults := Defaults{Profiles: profile.OpenDir(dir, quietLogger), Holidays: "JP",
		Home: weather.FixedHome{Place: weather.Place{Name: "配置里的家", Latitude: 35.6, Longitude: 139.7}}}
	service, err := Open(t.Context(), store, Admin{AccountID: admin.ID, OwnerPerson: admin.OwnerPerson, GuestPerson: admin.GuestPerson},
		defaults, quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{service: service, store: store, admin: admin, member: member}
}

// 管理员没在 App 里改过：资料、家、节假日都和只有配置时一样；别的账号什么都没有（配置写的是管理员的家）。
func TestDefaultsOnlyForAdmin(t *testing.T) {
	f := newFixture(t)
	owner, guest := accounts.PersonsOf(f.member.ID)
	if got := f.service.Profile("主人"); !strings.Contains(got, "猫") || !strings.Contains(got, "咖啡") || strings.Contains(got, "填写说明") {
		t.Errorf("主人：%q", got)
	}
	if got := f.service.Profile(""); !strings.Contains(got, "猫") || strings.Contains(got, "咖啡") {
		t.Errorf("认不出是谁：只有全家的：%q", got)
	}
	if got := f.service.Profile(owner) + f.service.Profile(guest); got != "" {
		t.Errorf("别的账号不套管理员的资料：%q", got)
	}
	if place, err := f.service.Home(t.Context(), "主人"); err != nil || place.Name != "配置里的家" {
		t.Errorf("管理员的家：%+v %v", place, err)
	}
	if _, err := f.service.Home(t.Context(), owner); !errors.Is(err, weather.ErrHomeNotSet) {
		t.Errorf("别的账号没设家：%v", err)
	}
	if _, _, ok := f.service.Known(guest); ok {
		t.Error("别的账号没设家时 Known 应是 false")
	}
	if f.service.holidayCountry("老大") != "JP" || f.service.holidayCountry(owner) != "" {
		t.Error("节假日：管理员家里的人用配置的国家，别的账号不用")
	}
	view := f.service.ProfileOf(f.admin.ID)
	if view.UpdatedAt != nil || !strings.Contains(view.Household, "填写说明") || view.Owner != "主人早上喝咖啡。" {
		t.Errorf("App 里看到的是资料文件原样的正文：%+v", view)
	}
	if home := f.service.HomeOf(f.admin.ID); home.UpdatedAt != nil || home.Place == nil || home.HolidaysCountry != "JP" {
		t.Errorf("App 里看到的是配置里的家：%+v", home)
	}
	if home := f.service.HomeOf(f.member.ID); home.Place != nil || home.HolidaysCountry != "" {
		t.Errorf("别的账号看到的是空的：%+v", home)
	}
}

// 在 App 里改了：马上生效、以库里的为准（配置不再管）；本人那段只给本人；重新打开（重启）后还在。
func TestSavedSettingsWin(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.service.ReplaceProfile(ctx, f.admin.ID, "新的全家资料", "新的本人资料<!-- 不发 -->"); err != nil {
		t.Fatal(err)
	}
	if got := f.service.Profile("主人"); !strings.Contains(got, "新的全家资料") || !strings.Contains(got, "新的本人资料") ||
		strings.Contains(got, "猫") || strings.Contains(got, "不发") {
		t.Errorf("主人：%q", got)
	}
	if got := f.service.Profile("老大"); !strings.Contains(got, "新的全家资料") || strings.Contains(got, "本人") {
		t.Errorf("家里别的人只有全家的：%q", got)
	}
	owner, guest := accounts.PersonsOf(f.member.ID)
	if _, err := f.service.ReplaceProfile(ctx, f.member.ID, "别人家", "别人自己"); err != nil {
		t.Fatal(err)
	}
	if got := f.service.Profile(owner); !strings.Contains(got, "别人家") || !strings.Contains(got, "〈本人〉\n别人自己") {
		t.Errorf("别的账号的本人：%q", got)
	}
	if got := f.service.Profile(guest); !strings.Contains(got, "别人家") || strings.Contains(got, "别人自己") {
		t.Errorf("别的账号的访客：%q", got)
	}
	osaka := &weather.Place{Name: "大阪市", CountryCode: "jp", Latitude: 34.69, Longitude: 135.5}
	if _, err := f.service.ReplaceHome(ctx, f.member.ID, osaka, "us"); err != nil {
		t.Fatal(err)
	}
	if place, err := f.service.Home(ctx, guest); err != nil || place.Name != "大阪市" || place.CountryCode != "JP" {
		t.Errorf("别的账号的家：%+v %v", place, err)
	}
	if f.service.holidayCountry(owner) != "US" {
		t.Error("别的账号的节假日国家")
	}
	if _, err := f.service.ReplaceHome(ctx, f.admin.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Home(ctx, "主人"); !errors.Is(err, weather.ErrHomeNotSet) {
		t.Errorf("管理员在 App 里清掉了家：不再用配置的：%v", err)
	}
	if f.service.holidayCountry("主人") != "" || f.service.HolidayNote("主人") != nil {
		t.Error("管理员清掉了节假日：不再用配置的")
	}
	reopened, err := Open(ctx, f.store, f.service.admin, f.service.defaults, quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Profile(owner); !strings.Contains(got, "别人自己") {
		t.Errorf("重启后还在：%q", got)
	}
	if view := reopened.ProfileOf(f.admin.ID); view.UpdatedAt == nil || view.Owner != "新的本人资料<!-- 不发 -->" {
		t.Errorf("App 里看到的是原样的：%+v", view)
	}
}

func TestReplaceHomeValidation(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []struct {
		place    *weather.Place
		holidays string
	}{{nil, "XX"}, {&weather.Place{Name: "错", Latitude: 91}, ""}, {&weather.Place{Name: "错", Longitude: -181}, ""}} {
		if _, err := f.service.ReplaceHome(t.Context(), f.member.ID, bad.place, bad.holidays); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v 应不合规：%v", bad, err)
		}
	}
}

// 没有账号库（App 的接口没开）：全照配置，和以前一样；不能改。
func TestWithoutAccounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, profile.HouseholdFile), []byte("家里有一只猫。"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := Open(context.Background(), nil, Admin{}, Defaults{Profiles: profile.OpenDir(dir, quietLogger), Holidays: "JP"}, quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Profile("谁都行"); !strings.Contains(got, "猫") {
		t.Errorf("资料：%q", got)
	}
	if service.holidayCountry("谁都行") != "JP" {
		t.Error("节假日")
	}
	if _, err := service.Home(t.Context(), "谁都行"); !errors.Is(err, weather.ErrHomeNotSet) {
		t.Errorf("没配家：%v", err)
	}
	if _, err := service.ReplaceProfile(t.Context(), "x", "a", "b"); err == nil {
		t.Error("没有账号库时不能改")
	}
	if _, err := Open(context.Background(), nil, Admin{}, Defaults{Holidays: "XX"}, quietLogger); err == nil {
		t.Error("不支持的节假日国家应报错")
	}
}
