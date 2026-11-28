"use client";

// 家的位置：搜地名（后端查）→ 选一个；节假日的国家从后端给的清单里选。只到城市，不用写门牌。
import { MapPin, Search } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import { getHome, listCountries, replaceHome, searchPlaces } from "@/api/generated/me/me";
import type { Country, Home, Place } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { showToast } from "@/components/overlay";
import { Button, Card, Failure, Group, Loading, Page } from "@/components/ui";
import { useApi } from "@/lib/useApi";

const loadHome = () => getHome();
const loadCountries = () => listCountries();

function placeName(place: Place) {
  return [place.name, place.region, place.country].filter(Boolean).join("，");
}

function HomeForm({ home, countries, reload }: { home: Home; countries: Country[]; reload: () => void }) {
  const t = useTranslations("home");
  const [query, setQuery] = useState("");
  const [found, setFound] = useState<Place[] | null>(null);
  const [busy, setBusy] = useState(false);
  const run = async (work: () => Promise<unknown>, done?: string) => {
    setBusy(true);
    try {
      await work();
      if (done) showToast(done);
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  const save = (update: { place: Place | null; holidays_country: string | null }, done: string) =>
    run(async () => {
      await replaceHome(update);
      setFound(null);
      setQuery("");
      reload();
    }, done);
  return (
    <>
      <Card title={t("current")}>
        <div className="between">
          <span className="hstack">
            <MapPin size={18} aria-hidden />
            {home.place ? placeName(home.place) : t("notSet")}
          </span>
          {home.place && (
            <Button small kind="plain" disabled={busy} onClick={() => void save({ place: null, holidays_country: home.holidays_country }, t("cleared"))}>
              {t("clear")}
            </Button>
          )}
        </div>
      </Card>
      <Card title={t("change")}>
        <form
          className="hstack"
          onSubmit={(event) => {
            event.preventDefault();
            if (query.trim()) void run(async () => setFound(await searchPlaces({ q: query.trim() })));
          }}
        >
          <label className="search m-0 grow">
            <Search size={18} aria-hidden />
            <input placeholder={t("searchPlaceholder")} aria-label={t("search")} value={query} onChange={(event) => setQuery(event.target.value)} />
          </label>
          <Button small type="submit" disabled={busy || !query.trim()}>
            {t("searchButton")}
          </Button>
        </form>
        {found && (
          <div className="mt-2.5">
            {found.length ? (
              <Group className="bg-tile">
                {found.map((place) => (
                  <div key={`${place.latitude},${place.longitude}`} className="row" style={{ cursor: "default" }}>
                    <span className="grow">{placeName(place)}</span>
                    <Button small disabled={busy} onClick={() => void save({ place, holidays_country: home.holidays_country }, t("saved"))}>
                      {t("choose")}
                    </Button>
                  </div>
                ))}
              </Group>
            ) : (
              <p className="sub p-3">{t("notFound")}</p>
            )}
          </div>
        )}
      </Card>
      <Card title={t("holidays")}>
        <select
          className="input"
          value={home.holidays_country ?? ""}
          disabled={busy}
          onChange={(event) => void save({ place: home.place, holidays_country: event.target.value || null }, event.target.value ? t("holidaysSaved") : t("holidaysOff"))}
        >
          <option value="">{t("noHolidays")}</option>
          {countries.map((country) => (
            <option key={country.code} value={country.code}>
              {country.name}
            </option>
          ))}
        </select>
        <p className="cap mt-2">{t("holidaysHint")}</p>
      </Card>
    </>
  );
}

export default function HomePage() {
  const t = useTranslations("home");
  const home = useApi(loadHome);
  const countries = useApi(loadCountries);
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {(home.loading || countries.loading) && <Loading />}
      <Failure message={home.error ?? countries.error} />
      {home.data && countries.data && <HomeForm home={home.data} countries={countries.data} reload={home.reload} />}
    </Page>
  );
}
