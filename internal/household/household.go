// Package household 是每个账号的「家」：主人资料（全家的、本人的）、家的位置、节假日的国家。
// 大脑（资料）、天气（家在哪）、节假日的提醒都按这一轮说话的人在这里取，App 的接口在这里改 —— 一个账号的这些设置只存一处。
//
// 管理员账号在 App 里没设过的项，用服务端配置的（资料文件 HARO_PROFILE_DIR、HARO_WEATHER_HOME、HARO_HOLIDAYS）：
// 和没有账号时一模一样；设过以后以账号库里的为准。别的账号没设时就是没有（服务端配置写的是管理员的家，不套给别人）。
// 设置在内存里有一份（启动时读、改的时候一起更新）：每轮出提示词、每轮的情况说明都不读库。
package household

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/holidays"
	"github.com/bukahou/haro/internal/profile"
	"github.com/bukahou/haro/internal/weather"
)

// Store 是存账号设置的地方（accounts.Store 实现）。
type Store interface {
	Profiles(ctx context.Context) (map[string]accounts.AccountProfile, error)
	ReplaceProfile(ctx context.Context, accountID, household, owner string) (accounts.AccountProfile, error)
	Homes(ctx context.Context) (map[string]accounts.AccountHome, error)
	ReplaceHome(ctx context.Context, accountID string, home accounts.AccountHome) (accounts.AccountHome, error)
}

// Admin 是管理员账号和它的两个人（沿用原来的声纹名和全家共用）。AccountID 为空 = 还没有管理员。
type Admin struct {
	AccountID   string
	OwnerPerson string
	GuestPerson string
}

// Defaults 是服务端配置里的家（给管理员账号没设过的项用）。都可以没有。
type Defaults struct {
	Profiles *profile.Store      // 资料文件；nil = 没配
	Home     weather.HomeLocator // 家的位置；nil = 没配
	Holidays string              // 节假日的国家；空 = 不告诉
}

// Service 是每个账号的家。实现 brain.ProfileSource、weather.HomeLocator；HolidayNote 是节假日的情况说明。
type Service struct {
	store    Store // nil = 没有账号库（App 的接口没开）：只用服务端的配置
	admin    Admin
	defaults Defaults
	logger   *slog.Logger

	mu        sync.RWMutex
	profiles  map[string]accounts.AccountProfile // 账号 → 原样的资料（App 里显示、编辑的）
	prepared  map[string]preparedProfile         // 账号 → 整理过、发给模型的
	homes     map[string]accounts.AccountHome
	calendars map[string]*holidays.Calendar // 国家 → 节假日（用到时才建）
}

type preparedProfile struct{ household, owner string }

// Open 读出所有账号的设置。store 为 nil 时只用 defaults。
func Open(ctx context.Context, store Store, admin Admin, defaults Defaults, logger *slog.Logger) (*Service, error) {
	service := &Service{store: store, admin: admin, defaults: defaults, logger: logger.With("component", "household"),
		profiles: map[string]accounts.AccountProfile{}, prepared: map[string]preparedProfile{},
		homes: map[string]accounts.AccountHome{}, calendars: map[string]*holidays.Calendar{}}
	if defaults.Holidays != "" && !holidays.Supported(defaults.Holidays) {
		_, err := holidays.ForCountry(defaults.Holidays)
		return nil, fmt.Errorf("HARO_HOLIDAYS：%w", err)
	}
	if store == nil {
		return service, nil
	}
	profiles, err := store.Profiles(ctx)
	if err != nil {
		return nil, err
	}
	homes, err := store.Homes(ctx)
	if err != nil {
		return nil, err
	}
	for id, saved := range profiles {
		service.profiles[id] = saved
		service.prepared[id] = service.prepare(id, saved)
	}
	service.homes = homes
	service.noteOverrides()
	return service, nil
}

// noteOverrides 启动时提示：管理员在 App 里设过的项，服务端的配置对它已经不生效（可以从配置里删掉）。
func (s *Service) noteOverrides() {
	if s.admin.AccountID == "" {
		return
	}
	if _, ok := s.profiles[s.admin.AccountID]; ok && s.defaults.Profiles != nil {
		s.logger.Info("主人资料：管理员在 App 里改过，资料文件（HARO_PROFILE_DIR）对管理员不再生效，可以从配置里删掉")
	}
	if _, ok := s.homes[s.admin.AccountID]; ok && (s.defaults.Home != nil || s.defaults.Holidays != "") {
		s.logger.Info("家：管理员在 App 里设过，HARO_WEATHER_HOME、HARO_HOLIDAYS 对管理员不再生效，可以从配置里删掉")
	}
}

// resolve 找出这个人属于哪个账号、是不是账号本人。终端上声纹认出的别的名字（家里人）归管理员账号、不算本人
// （终端现在都归管理员）；没有管理员时 accountID 为空（只用服务端的配置）。
func (s *Service) resolve(person string) (accountID string, owner bool) {
	switch {
	case s.admin.AccountID != "" && person == s.admin.OwnerPerson:
		return s.admin.AccountID, true
	case s.admin.AccountID != "" && person == s.admin.GuestPerson:
		return s.admin.AccountID, false
	}
	if id, isOwner, ok := accounts.ParsePerson(person); ok {
		return id, isOwner
	}
	return s.admin.AccountID, false
}

// usesDefaults 判断这个账号没设过的项用不用服务端的配置：管理员账号、或者还没有账号（accountID 为空）才用。
func (s *Service) usesDefaults(accountID string) bool {
	return accountID == "" || accountID == s.admin.AccountID
}

// prepare 把一个账号的资料整理成发给模型的样子；太长的截断并警告。
func (s *Service) prepare(accountID string, saved accounts.AccountProfile) preparedProfile {
	household, cutHousehold := profile.Prepare(saved.Household)
	owner, cutOwner := profile.Prepare(saved.Owner)
	if cutHousehold || cutOwner {
		s.logger.Warn("主人资料太长，超出的部分不发给模型", "account", accountID, "limit", profile.MaxRunes)
	}
	return preparedProfile{household: household, owner: owner}
}

// Profile 实现 brain.ProfileSource：这个人所在账号的「全家」资料 + 是本人时本人的那份。
func (s *Service) Profile(person string) string {
	accountID, owner := s.resolve(person)
	s.mu.RLock()
	prepared, saved := s.prepared[accountID]
	s.mu.RUnlock()
	switch {
	case saved:
		own := ""
		if owner {
			own = prepared.owner
		}
		return profile.Compose(prepared.household, ownerLabel(person), own)
	case s.usesDefaults(accountID) && s.defaults.Profiles != nil:
		return s.defaults.Profiles.Profile(person)
	}
	return ""
}

// ownerLabel 是提示词里本人那段的标题：沿用的声纹名照写；新账号的人（account:…）写「本人」。
func ownerLabel(person string) string {
	if _, _, ok := accounts.ParsePerson(person); ok {
		return "本人"
	}
	return person
}

// ProfileView 是 App 里看到的资料：原样的两段；UpdatedAt 为 nil = 还没在 App 里改过（管理员账号这时是资料文件里的）。
type ProfileView struct {
	Household string
	Owner     string
	UpdatedAt *time.Time
}

// ProfileOf 是一个账号在 App 里看到的资料。
func (s *Service) ProfileOf(accountID string) ProfileView {
	s.mu.RLock()
	saved, ok := s.profiles[accountID]
	s.mu.RUnlock()
	switch {
	case ok:
		updated := saved.UpdatedAt
		return ProfileView{Household: saved.Household, Owner: saved.Owner, UpdatedAt: &updated}
	case s.usesDefaults(accountID) && s.defaults.Profiles != nil:
		texts := s.defaults.Profiles.Raw(s.admin.OwnerPerson)
		return ProfileView{Household: texts.Household, Owner: texts.Owner}
	}
	return ProfileView{}
}

// ReplaceProfile 整份替换一个账号的资料（下一轮对话就用新的）。
func (s *Service) ReplaceProfile(ctx context.Context, accountID, household, owner string) (ProfileView, error) {
	if s.store == nil {
		return ProfileView{}, fmt.Errorf("没有账号库，资料只能改资料文件")
	}
	saved, err := s.store.ReplaceProfile(ctx, accountID, household, owner)
	if err != nil {
		return ProfileView{}, err
	}
	prepared := s.prepare(accountID, saved)
	s.mu.Lock()
	s.profiles[accountID], s.prepared[accountID] = saved, prepared
	s.mu.Unlock()
	s.logger.Info("主人资料改了", "account", accountID)
	return s.ProfileOf(accountID), nil
}

// ErrInvalid 是要存的设置不合规（App 的接口回 400）。
var ErrInvalid = errors.New("设置不合规")
