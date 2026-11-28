package accounts

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AccountProfile 是一个账号的主人资料（两份）。
type AccountProfile struct {
	Household string // 全家（这个账号家里的人、认不出声音的也带上）
	Owner     string // 本人（认出是账号本人时才带上）
	UpdatedAt time.Time
}

// HomePlace 是家的位置。
type HomePlace struct {
	Name, Region, Country string
	CountryCode           string // ISO 3166-1 两位；可以为空（只知道坐标时）
	Latitude, Longitude   float64
}

// AccountHome 是一个账号的家：位置（nil = 没设）和节假日的国家（空 = 不告诉）。
type AccountHome struct {
	Place           *HomePlace
	HolidaysCountry string
	UpdatedAt       time.Time
}

// Profiles 是所有在 App 里设过资料的账号（账号编号 → 资料）。
func (s *Store) Profiles(ctx context.Context) (map[string]AccountProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, household, owner, updated_at FROM profiles`)
	if err != nil {
		return nil, fmt.Errorf("读主人资料: %w", err)
	}
	defer rows.Close()
	profiles := map[string]AccountProfile{}
	for rows.Next() {
		var id string
		var profile AccountProfile
		var updated int64
		if err := rows.Scan(&id, &profile.Household, &profile.Owner, &updated); err != nil {
			return nil, fmt.Errorf("读主人资料: %w", err)
		}
		profile.UpdatedAt = time.Unix(updated, 0).UTC()
		profiles[id] = profile
	}
	return profiles, rows.Err()
}

// ReplaceProfile 整份替换一个账号的主人资料。
func (s *Store) ReplaceProfile(ctx context.Context, accountID, household, owner string) (AccountProfile, error) {
	profile := AccountProfile{Household: household, Owner: owner, UpdatedAt: s.now().UTC().Truncate(time.Second)}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO profiles (account_id, household, owner, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET household = excluded.household, owner = excluded.owner, updated_at = excluded.updated_at`,
		accountID, household, owner, profile.UpdatedAt.Unix()); err != nil {
		return AccountProfile{}, fmt.Errorf("存主人资料: %w", err)
	}
	return profile, nil
}

// Homes 是所有在 App 里设过家的账号（账号编号 → 家）。
func (s *Store) Homes(ctx context.Context) (map[string]AccountHome, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, place_name, place_region, place_country, country_code, latitude, longitude,
		holidays_country, updated_at FROM homes`)
	if err != nil {
		return nil, fmt.Errorf("读家的设置: %w", err)
	}
	defer rows.Close()
	homes := map[string]AccountHome{}
	for rows.Next() {
		var id string
		var name, region, country, code, holidays sql.NullString
		var latitude, longitude sql.NullFloat64
		var updated int64
		if err := rows.Scan(&id, &name, &region, &country, &code, &latitude, &longitude, &holidays, &updated); err != nil {
			return nil, fmt.Errorf("读家的设置: %w", err)
		}
		home := AccountHome{HolidaysCountry: holidays.String, UpdatedAt: time.Unix(updated, 0).UTC()}
		if latitude.Valid && longitude.Valid {
			home.Place = &HomePlace{Name: name.String, Region: region.String, Country: country.String, CountryCode: code.String,
				Latitude: latitude.Float64, Longitude: longitude.Float64}
		}
		homes[id] = home
	}
	return homes, rows.Err()
}

// ReplaceHome 整份替换一个账号的家。
func (s *Store) ReplaceHome(ctx context.Context, accountID string, home AccountHome) (AccountHome, error) {
	home.UpdatedAt = s.now().UTC().Truncate(time.Second)
	var name, region, country, code, holidays, latitude, longitude any
	if place := home.Place; place != nil {
		name, region, country, code = nullable(place.Name), nullable(place.Region), nullable(place.Country), nullable(place.CountryCode)
		latitude, longitude = place.Latitude, place.Longitude
	}
	holidays = nullable(home.HolidaysCountry)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO homes (account_id, place_name, place_region, place_country, country_code, latitude,
		longitude, holidays_country, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET place_name = excluded.place_name, place_region = excluded.place_region,
		place_country = excluded.place_country, country_code = excluded.country_code, latitude = excluded.latitude,
		longitude = excluded.longitude, holidays_country = excluded.holidays_country, updated_at = excluded.updated_at`,
		accountID, name, region, country, code, latitude, longitude, holidays, home.UpdatedAt.Unix()); err != nil {
		return AccountHome{}, fmt.Errorf("存家的设置: %w", err)
	}
	return home, nil
}

// nullable 把空字符串存成 NULL。
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
