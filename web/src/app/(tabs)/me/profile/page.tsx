"use client";

// 主人资料：两段（全家的、我自己的），每段的字数上限由后端给（max_chars）。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import { getProfile, replaceProfile } from "@/api/generated/me/me";
import type { Profile } from "@/api/generated/model";
import { Button, Card, Failure, Loading, Note, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useAction, useApi } from "@/lib/useApi";

const loadProfile = () => getProfile();

function ProfileForm({ profile, reload }: { profile: Profile; reload: () => void }) {
  const t = useTranslations("profile");
  const tc = useTranslations("common");
  const time = useTimeFormat();
  const [household, setHousehold] = useState(profile.household);
  const [owner, setOwner] = useState(profile.owner);
  const [saved, setSaved] = useState(false);
  const save = useAction(
    useCallback(async () => {
      await replaceProfile({ household, owner });
      setSaved(true);
    }, [household, owner]),
    reload,
  );
  const field = (label: string, value: string, change: (value: string) => void) => (
    <label className="flex flex-col gap-1">
      <span className="text-sm text-muted">{label}</span>
      <textarea
        className="min-h-40 rounded-lg border border-border bg-surface p-3"
        value={value}
        maxLength={profile.max_chars}
        onChange={(e) => {
          change(e.target.value);
          setSaved(false);
        }}
      />
      <span className="self-end text-xs text-muted">{t("chars", { count: [...value].length, max: profile.max_chars })}</span>
    </label>
  );
  return (
    <Card>
      <div className="flex flex-col gap-4">
        {field(t("household"), household, setHousehold)}
        {field(t("owner"), owner, setOwner)}
        <div className="flex items-center gap-3">
          <Button kind="primary" disabled={save.busy} onClick={() => void save.act()}>
            {tc("save")}
          </Button>
          {saved && <span className="text-sm text-green-600">{tc("saved")}</span>}
          {profile.updated_at && <Note>{t("updated", { time: time.dateTime(profile.updated_at) })}</Note>}
        </div>
        <Failure message={save.error} />
      </div>
    </Card>
  );
}

export default function ProfilePage() {
  const t = useTranslations("profile");
  const { data, error, loading, reload } = useApi(loadProfile);
  return (
    <Page title={t("title")} back="/me">
      <Note>{t("hint")}</Note>
      {loading && <Loading />}
      <Failure message={error} />
      {data && <ProfileForm key={data.updated_at ?? "new"} profile={data} reload={reload} />}
    </Page>
  );
}
