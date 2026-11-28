"use client";

// 家的位置：搜地名（后端查）→ 选一个；节假日的国家从后端给的清单里选。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import { getHome, listCountries, replaceHome, searchPlaces } from "@/api/generated/me/me";
import type { Country, Home, Place } from "@/api/generated/model";
import { Button, Card, Failure, Loading, Note, Page } from "@/components/ui";
import { useAction, useApi } from "@/lib/useApi";

const loadHome = () => getHome();
const loadCountries = () => listCountries();

function placeName(place: Place) {
  return [place.name, place.region, place.country].filter(Boolean).join("，");
}

function HomeForm({ home, countries, reload }: { home: Home; countries: Country[]; reload: () => void }) {
  const t = useTranslations("home");
  const tc = useTranslations("common");
  const [query, setQuery] = useState("");
  const [found, setFound] = useState<Place[] | null>(null);
  const search = useAction(useCallback(async (q: string) => setFound(await searchPlaces({ q })), []));
  const save = useAction(useCallback((update: { place: Place | null; holidays_country: string | null }) => replaceHome(update), []), reload);
  return (
    <>
      <Card title={t("current")}>
        <p>{home.place ? placeName(home.place) : t("notSet")}</p>
        {home.place && (
          <div className="mt-2">
            <Button onClick={() => void save.act({ place: null, holidays_country: home.holidays_country })}>{t("clear")}</Button>
          </div>
        )}
      </Card>
      <Card title={t("search")}>
        <form
          className="flex gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            if (query.trim()) void search.act(query.trim());
          }}
        >
          <input className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-3 py-2" value={query} onChange={(e) => setQuery(e.target.value)} />
          <Button type="submit" disabled={search.busy}>
            {t("searchButton")}
          </Button>
        </form>
        {found && (
          <ul className="mt-3 divide-y divide-border">
            {found.length === 0 && <Note>{tc("none")}</Note>}
            {found.map((place) => (
              <li key={`${place.latitude},${place.longitude}`} className="flex items-center justify-between gap-3 py-2">
                <span className="text-sm">{placeName(place)}</span>
                <Button disabled={save.busy} onClick={() => void save.act({ place, holidays_country: home.holidays_country })}>
                  {t("choose")}
                </Button>
              </li>
            ))}
          </ul>
        )}
        <Failure message={search.error} />
      </Card>
      <Card title={t("holidays")}>
        <select
          className="w-full rounded-lg border border-border bg-surface px-3 py-2"
          value={home.holidays_country ?? ""}
          disabled={save.busy}
          onChange={(e) => void save.act({ place: home.place, holidays_country: e.target.value || null })}
        >
          <option value="">{t("noHolidays")}</option>
          {countries.map((country) => (
            <option key={country.code} value={country.code}>
              {country.name}
            </option>
          ))}
        </select>
      </Card>
      <Failure message={save.error} />
    </>
  );
}

export default function HomePage() {
  const t = useTranslations("home");
  const home = useApi(loadHome);
  const countries = useApi(loadCountries);
  return (
    <Page title={t("title")} back="/me">
      <Note>{t("hint")}</Note>
      {(home.loading || countries.loading) && <Loading />}
      <Failure message={home.error ?? countries.error} />
      {home.data && countries.data && <HomeForm home={home.data} countries={countries.data} reload={home.reload} />}
    </Page>
  );
}
