"use client";

// 主人资料：两段（全家的、我自己的），haro 每轮都会看到；每段的字数上限由后端给（max_chars）。
import { useTranslations } from "next-intl";
import { useState } from "react";

import { getProfile, replaceProfile } from "@/api/generated/me/me";
import type { Profile } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { showToast } from "@/components/overlay";
import { Button, Failure, Field, Loading, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadProfile = () => getProfile();

function ProfileForm({ profile, reload }: { profile: Profile; reload: () => void }) {
  const t = useTranslations();
  const time = useTimeFormat();
  const [household, setHousehold] = useState(profile.household);
  const [owner, setOwner] = useState(profile.owner);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      await replaceProfile({ household, owner });
      showToast(t("profile.saved"));
      reload();
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  const area = (label: string, value: string, change: (value: string) => void) => (
    <Field label={label} note={t("profile.chars", { count: [...value].length, max: profile.max_chars })}>
      <textarea className="input" value={value} maxLength={profile.max_chars} onChange={(event) => change(event.target.value)} />
    </Field>
  );
  return (
    <div className="card stack" style={{ gap: 16 }}>
      {area(t("profile.household"), household, setHousehold)}
      {area(t("profile.owner"), owner, setOwner)}
      <div className="between">
        <span className="cap">{profile.updated_at && t("profile.updated", { time: time.dateTime(profile.updated_at) })}</span>
        <Button kind="solid" disabled={busy} onClick={() => void save()}>
          {t("common.save")}
        </Button>
      </div>
    </div>
  );
}

export default function ProfilePage() {
  const t = useTranslations("profile");
  const { data, error, loading, reload } = useApi(loadProfile);
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && <ProfileForm key={data.updated_at ?? "new"} profile={data} reload={reload} />}
    </Page>
  );
}
