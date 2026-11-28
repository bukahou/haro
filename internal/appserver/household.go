package appserver

import (
	"context"
	"errors"
	"net/http"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/holidays"
	"github.com/bukahou/haro/internal/household"
	"github.com/bukahou/haro/internal/weather"
)

// placeSearchLimit 是搜地名时最多给几个候选。
const placeSearchLimit = 10

// Household 是每个账号的主人资料和家（household.Service 实现）。
type Household interface {
	ProfileOf(accountID string) household.ProfileView
	ReplaceProfile(ctx context.Context, accountID, household, owner string) (household.ProfileView, error)
	HomeOf(accountID string) household.HomeView
	ReplaceHome(ctx context.Context, accountID string, place *weather.Place, holidaysCountry string) (household.HomeView, error)
}

// PlaceFinder 按地名找地方（天气用的地名库）。
type PlaceFinder interface {
	FindPlaces(ctx context.Context, query weather.PlaceQuery, limit int) ([]weather.Place, error)
}

// GetProfile 实现 appapi.StrictServerInterface。
func (s *Server) GetProfile(ctx context.Context, _ appapi.GetProfileRequestObject) (appapi.GetProfileResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Household == nil {
		return s.notImplemented(ctx), nil
	}
	return appapi.GetProfile200JSONResponse(s.profileOf(s.opts.Household.ProfileOf(account.ID))), nil
}

// ReplaceProfile 实现 appapi.StrictServerInterface。
func (s *Server) ReplaceProfile(ctx context.Context, request appapi.ReplaceProfileRequestObject) (appapi.ReplaceProfileResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Household == nil {
		return s.notImplemented(ctx), nil
	}
	view, err := s.opts.Household.ReplaceProfile(ctx, account.ID, request.Body.Household, request.Body.Owner)
	if err != nil {
		return s.internalError(ctx, "改主人资料", err), nil
	}
	return appapi.ReplaceProfile200JSONResponse(s.profileOf(view)), nil
}

func (s *Server) profileOf(view household.ProfileView) appapi.Profile {
	return appapi.Profile{Household: view.Household, Owner: view.Owner, MaxChars: s.opts.ProfileMaxChars, UpdatedAt: view.UpdatedAt}
}

// GetHome 实现 appapi.StrictServerInterface。
func (s *Server) GetHome(ctx context.Context, _ appapi.GetHomeRequestObject) (appapi.GetHomeResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Household == nil {
		return s.notImplemented(ctx), nil
	}
	return appapi.GetHome200JSONResponse(homeOf(s.opts.Household.HomeOf(account.ID))), nil
}

// ReplaceHome 实现 appapi.StrictServerInterface。
func (s *Server) ReplaceHome(ctx context.Context, request appapi.ReplaceHomeRequestObject) (appapi.ReplaceHomeResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Household == nil {
		return s.notImplemented(ctx), nil
	}
	var place *weather.Place
	if request.Body.Place != nil {
		converted := fromAPIPlace(*request.Body.Place)
		place = &converted
	}
	view, err := s.opts.Household.ReplaceHome(ctx, account.ID, place, deref(request.Body.HolidaysCountry))
	switch {
	case errors.Is(err, household.ErrInvalid):
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": err.Error()}), nil
	case err != nil:
		return s.internalError(ctx, "改家的设置", err), nil
	}
	return appapi.ReplaceHome200JSONResponse(homeOf(view)), nil
}

func homeOf(view household.HomeView) appapi.Home {
	home := appapi.Home{HolidaysCountry: optional(view.HolidaysCountry), UpdatedAt: view.UpdatedAt}
	if view.Place != nil {
		place := toAPIPlace(*view.Place)
		home.Place = &place
	}
	return home
}

// SearchPlaces 实现 appapi.StrictServerInterface：名字按 Accept-Language 选的语言。
func (s *Server) SearchPlaces(ctx context.Context, request appapi.SearchPlacesRequestObject) (appapi.SearchPlacesResponseObject, error) {
	if _, denied := s.me(ctx); denied != nil {
		return *denied, nil
	}
	if s.opts.Places == nil {
		return s.notImplemented(ctx), nil
	}
	found, err := s.opts.Places.FindPlaces(ctx, weather.PlaceQuery{Name: request.Params.Q, Language: languageFrom(ctx)}, placeSearchLimit)
	if err != nil {
		return s.internalError(ctx, "搜地名", err), nil
	}
	places := make(appapi.SearchPlaces200JSONResponse, 0, len(found))
	for _, place := range found {
		places = append(places, toAPIPlace(place))
	}
	return places, nil
}

// ListCountries 实现 appapi.StrictServerInterface：支持节假日的国家，名字和排序按 Accept-Language 选的语言。
func (s *Server) ListCountries(ctx context.Context, _ appapi.ListCountriesRequestObject) (appapi.ListCountriesResponseObject, error) {
	if _, denied := s.me(ctx); denied != nil {
		return *denied, nil
	}
	tag := language.Make(languageFrom(ctx))
	names := display.Regions(tag)
	countries := make(appapi.ListCountries200JSONResponse, 0)
	byName := map[string]string{}
	var labels []string
	for _, code := range holidays.Countries() {
		name := code
		if region, err := language.ParseRegion(code); err == nil && names != nil {
			if localized := names.Name(region); localized != "" {
				name = localized
			}
		}
		byName[name] = code
		labels = append(labels, name)
	}
	collate.New(tag).SortStrings(labels)
	for _, name := range labels {
		countries = append(countries, appapi.Country{Code: byName[name], Name: name})
	}
	return countries, nil
}

func toAPIPlace(place weather.Place) appapi.Place {
	return appapi.Place{Name: place.Name, Region: optional(place.Region), Country: optional(place.Country),
		CountryCode: optional(place.CountryCode), Latitude: place.Latitude, Longitude: place.Longitude}
}

func fromAPIPlace(place appapi.Place) weather.Place {
	return weather.Place{Name: place.Name, Region: deref(place.Region), Country: deref(place.Country),
		CountryCode: deref(place.CountryCode), Latitude: place.Latitude, Longitude: place.Longitude}
}

// optional 把空字符串写成 null。
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
