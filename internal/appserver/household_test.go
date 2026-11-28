package appserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/household"
	"github.com/bukahou/haro/internal/weather"
)

// fakePlaces 记下按什么语言搜的，给一个候选。
type fakePlaces struct{ language string }

func (f *fakePlaces) FindPlaces(_ context.Context, query weather.PlaceQuery, _ int) ([]weather.Place, error) {
	f.language = query.Language
	return []weather.Place{{Name: "大阪市", Region: "大阪府", CountryCode: "JP", Latitude: 34.69, Longitude: 135.5}}, nil
}

func newHouseholdFixture(t *testing.T) (fixture, *fakePlaces) {
	t.Helper()
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, err := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.IssueTokens(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := household.Open(ctx, store, household.Admin{AccountID: admin.ID, OwnerPerson: admin.OwnerPerson,
		GuestPerson: admin.GuestPerson}, household.Defaults{Holidays: "JP"}, quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	places := &fakePlaces{}
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY", MinAppVersion: "1.0.0",
		ProfileMaxChars: 10000, Household: service, Places: places, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return fixture{server: server, store: store, admin: admin, adminKey: pair}, places
}

// list 发一个回数组的请求。
func (f fixture) list(t *testing.T, path, language string) []map[string]any {
	t.Helper()
	request, _ := http.NewRequest("GET", f.server.URL+path, nil)
	request.Header.Set("Authorization", "Bearer "+f.adminKey.AccessToken)
	request.Header.Set("Accept-Language", language)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var items []map[string]any
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&items) != nil {
		t.Fatalf("%s：%d", path, response.StatusCode)
	}
	return items
}

// 主人资料：没改过时是空的（这里没配资料文件）；整份替换后马上读得到；超过上限的给 400。
func TestProfileEndpoints(t *testing.T) {
	f, _ := newHouseholdFixture(t)
	key := f.adminKey.AccessToken
	if status, body := f.call(t, "GET", "/api/v1/me/profile", key, ""); status != 200 || body["updated_at"] != nil || body["max_chars"] != float64(10000) {
		t.Errorf("没改过：%d %v", status, body)
	}
	if status, body := f.call(t, "PUT", "/api/v1/me/profile", key, `{"household":"家里有猫","owner":"早上喝咖啡"}`); status != 200 ||
		body["household"] != "家里有猫" || body["owner"] != "早上喝咖啡" || body["updated_at"] == nil {
		t.Errorf("替换：%d %v", status, body)
	}
	if status, body := f.call(t, "GET", "/api/v1/me/profile", key, ""); status != 200 || body["owner"] != "早上喝咖啡" {
		t.Errorf("读回来：%d %v", status, body)
	}
	tooLong := `{"household":"` + strings.Repeat("字", 10001) + `","owner":""}`
	if status, body := f.call(t, "PUT", "/api/v1/me/profile", key, tooLong); status != 400 || body["code"] != "validation_failed" {
		t.Errorf("太长：%d %v", status, body)
	}
}

// 家：没设过时管理员看到配置里的节假日国家；整份替换；不支持的国家、坐标出界给 400。
func TestHomeEndpoints(t *testing.T) {
	f, _ := newHouseholdFixture(t)
	key := f.adminKey.AccessToken
	if status, body := f.call(t, "GET", "/api/v1/me/home", key, ""); status != 200 || body["holidays_country"] != "JP" ||
		body["place"] != nil || body["updated_at"] != nil {
		t.Errorf("没设过：%d %v", status, body)
	}
	osaka := `{"place":{"name":"大阪市","country_code":"JP","latitude":34.69,"longitude":135.5},"holidays_country":"US"}`
	status, body := f.call(t, "PUT", "/api/v1/me/home", key, osaka)
	place, _ := body["place"].(map[string]any)
	if status != 200 || body["holidays_country"] != "US" || place["name"] != "大阪市" || body["updated_at"] == nil {
		t.Errorf("替换：%d %v", status, body)
	}
	for _, bad := range []string{`{"place":null,"holidays_country":"XX"}`,
		`{"place":{"name":"错","latitude":91,"longitude":0},"holidays_country":null}`} {
		if status, body := f.call(t, "PUT", "/api/v1/me/home", key, bad); status != 400 || body["code"] != "validation_failed" {
			t.Errorf("%s：%d %v", bad, status, body)
		}
	}
	if status, body := f.call(t, "PUT", "/api/v1/me/home", key, `{"place":null,"holidays_country":null}`); status != 200 ||
		body["place"] != nil || body["holidays_country"] != nil {
		t.Errorf("清掉：%d %v", status, body)
	}
}

// 搜地名按 Accept-Language 选的语言；国家的名字、排序也按它。没有「家」那一层时这几个接口回 501。
func TestPlacesAndCountries(t *testing.T) {
	f, places := newHouseholdFixture(t)
	if found := f.list(t, "/api/v1/places?q=%E5%A4%A7%E9%98%AA", "ja"); len(found) != 1 || found[0]["country_code"] != "JP" || places.language != "ja" {
		t.Errorf("搜地名：%v（语言 %q）", found, places.language)
	}
	names := map[string]map[string]string{}
	for _, language := range []string{"ja", "en", "zh"} {
		names[language] = map[string]string{}
		for _, country := range f.list(t, "/api/v1/countries", language) {
			names[language][country["code"].(string)] = country["name"].(string)
		}
	}
	if names["ja"]["JP"] != "日本" || names["en"]["JP"] != "Japan" || names["zh"]["US"] != "美国" || len(names["en"]) < 40 {
		t.Errorf("国家的名字：ja=%q en=%q zh=%q（%d 个）", names["ja"]["JP"], names["en"]["JP"], names["zh"]["US"], len(names["en"]))
	}
	plain := newFixture(t)
	if status, body := plain.call(t, "GET", "/api/v1/me/profile", plain.adminKey.AccessToken, ""); status != 501 || body["code"] != "not_implemented" {
		t.Errorf("没有「家」那一层：%d %v", status, body)
	}
}
