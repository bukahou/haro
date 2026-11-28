package household

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/holidays"
	"github.com/bukahou/haro/internal/weather"
)

// savedHome 是这个人所在账号在 App 里设过的家；没设过时 ok = false。
func (s *Service) savedHome(person string) (accountID string, home accounts.AccountHome, ok bool) {
	accountID, _ = s.resolve(person)
	s.mu.RLock()
	home, ok = s.homes[accountID]
	s.mu.RUnlock()
	return accountID, home, ok
}

// Home 实现 weather.HomeLocator：这个人所在账号的家；没设时返回 weather.ErrHomeNotSet。
func (s *Service) Home(ctx context.Context, person string) (weather.Place, error) {
	accountID, home, ok := s.savedHome(person)
	switch {
	case ok && home.Place != nil:
		return toWeatherPlace(*home.Place), nil
	case !ok && s.usesDefaults(accountID) && s.defaults.Home != nil:
		return s.defaults.Home.Home(ctx, person)
	}
	return weather.Place{}, weather.ErrHomeNotSet
}

// Known 实现 weather.HomeLocator（不发网络请求）。
func (s *Service) Known(person string) (weather.Place, bool, bool) {
	accountID, home, ok := s.savedHome(person)
	switch {
	case ok && home.Place != nil:
		return toWeatherPlace(*home.Place), false, true
	case !ok && s.usesDefaults(accountID) && s.defaults.Home != nil:
		return s.defaults.Home.Known(person)
	}
	return weather.Place{}, false, false
}

// holidayCountry 是这个人所在账号要告诉 haro 的节假日国家；空 = 不告诉。
func (s *Service) holidayCountry(person string) string {
	accountID, home, ok := s.savedHome(person)
	switch {
	case ok:
		return home.HolidaysCountry
	case s.usesDefaults(accountID):
		return s.defaults.Holidays
	}
	return ""
}

// HolidayNote 是节假日的情况说明（brain.BriefLines）：这个人所在账号设了国家、今天或明天是那里的节假日时各一行。
func (s *Service) HolidayNote(person string) []string {
	country := s.holidayCountry(person)
	if country == "" {
		return nil
	}
	s.mu.Lock()
	calendar, ok := s.calendars[country]
	if !ok {
		var err error
		if calendar, err = holidays.ForCountry(country); err != nil { // 存进去之前校验过，不该发生
			s.mu.Unlock()
			s.logger.Warn("节假日：国家代码不认识", "country", country, "err", err)
			return nil
		}
		s.calendars[country] = calendar
	}
	s.mu.Unlock()
	return calendar.Brief(person)
}

// HomeView 是 App 里看到的家：位置（nil = 没设）、节假日的国家（空 = 不告诉）；
// UpdatedAt 为 nil = 还没在 App 里设过（管理员账号这时是服务端配置里的）。
type HomeView struct {
	Place           *weather.Place
	HolidaysCountry string
	UpdatedAt       *time.Time
}

// HomeOf 是一个账号在 App 里看到的家。
func (s *Service) HomeOf(accountID string) HomeView {
	s.mu.RLock()
	home, ok := s.homes[accountID]
	s.mu.RUnlock()
	if ok {
		view := HomeView{HolidaysCountry: home.HolidaysCountry}
		updated := home.UpdatedAt
		view.UpdatedAt = &updated
		if home.Place != nil {
			place := toWeatherPlace(*home.Place)
			view.Place = &place
		}
		return view
	}
	if !s.usesDefaults(accountID) {
		return HomeView{}
	}
	view := HomeView{HolidaysCountry: s.defaults.Holidays}
	if s.defaults.Home != nil {
		if place, _, known := s.defaults.Home.Known(s.admin.OwnerPerson); known {
			view.Place = &place
		}
	}
	return view
}

// ReplaceHome 整份替换一个账号的家（下一轮就用新的）。节假日的国家要是支持的；位置的坐标要在范围里。
func (s *Service) ReplaceHome(ctx context.Context, accountID string, place *weather.Place, holidaysCountry string) (HomeView, error) {
	if s.store == nil {
		return HomeView{}, fmt.Errorf("没有账号库，家只能在服务端的配置里改")
	}
	holidaysCountry = strings.ToUpper(strings.TrimSpace(holidaysCountry))
	if holidaysCountry != "" && !holidays.Supported(holidaysCountry) {
		return HomeView{}, fmt.Errorf("%w：节假日不支持这个国家（%s）", ErrInvalid, holidaysCountry)
	}
	home := accounts.AccountHome{HolidaysCountry: holidaysCountry}
	if place != nil {
		if place.Latitude < -90 || place.Latitude > 90 || place.Longitude < -180 || place.Longitude > 180 {
			return HomeView{}, fmt.Errorf("%w：坐标不在范围里", ErrInvalid)
		}
		home.Place = &accounts.HomePlace{Name: place.Name, Region: place.Region, Country: place.Country,
			CountryCode: strings.ToUpper(place.CountryCode), Latitude: place.Latitude, Longitude: place.Longitude}
	}
	saved, err := s.store.ReplaceHome(ctx, accountID, home)
	if err != nil {
		return HomeView{}, err
	}
	s.mu.Lock()
	s.homes[accountID] = saved
	s.mu.Unlock()
	s.logger.Info("家改了", "account", accountID, "place_set", place != nil, "holidays", holidaysCountry)
	return s.HomeOf(accountID), nil
}

func toWeatherPlace(place accounts.HomePlace) weather.Place {
	return weather.Place{Name: place.Name, Region: place.Region, Country: place.Country, CountryCode: place.CountryCode,
		Latitude: place.Latitude, Longitude: place.Longitude}
}
