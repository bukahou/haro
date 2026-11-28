"use client";

// 管理员：邀请码。生成（备注、多少小时后过期）后码和登录链接只显示这一次；列表看状态（没用 / 用过 / 过期 / 撤销）、撤销没用的。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import { createInvite, listInvites, revokeInvite } from "@/api/generated/admin/admin";
import type { CreatedInvite, Invite } from "@/api/generated/model";
import { Button, Card, Failure, Loading, Note, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useAction, useApi } from "@/lib/useApi";

const loadInvites = () => listInvites();

function Copyable({ value }: { value: string }) {
  const tc = useTranslations("common");
  const [copied, setCopied] = useState(false);
  return (
    <div className="flex items-center gap-2">
      <code className="min-w-0 flex-1 break-all rounded bg-surface px-2 py-1 text-sm">{value}</code>
      <Button onClick={() => void navigator.clipboard.writeText(value).then(() => setCopied(true))}>{copied ? tc("copied") : tc("copy")}</Button>
    </div>
  );
}

function CreateForm({ reload }: { reload: () => void }) {
  const t = useTranslations("admin.invites");
  const [note, setNote] = useState("");
  const [hours, setHours] = useState(168);
  const [created, setCreated] = useState<CreatedInvite | null>(null);
  const create = useAction(
    useCallback(async () => setCreated(await createInvite({ note: note.trim() || undefined, expires_in_hours: hours })), [note, hours]),
    reload,
  );
  return (
    <Card title={t("create")}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          void create.act();
        }}
      >
        <label className="flex flex-col gap-1">
          <span className="text-sm text-muted">{t("note")}</span>
          <input className="rounded-lg border border-border bg-surface px-3 py-2" value={note} maxLength={200} onChange={(e) => setNote(e.target.value)} />
        </label>
        <label className="flex flex-col gap-1">
          <span className="text-sm text-muted">{t("hours")}</span>
          <input
            type="number"
            min={1}
            max={720}
            className="w-32 rounded-lg border border-border bg-surface px-3 py-2"
            value={hours}
            onChange={(e) => setHours(Number(e.target.value))}
          />
        </label>
        <div>
          <Button type="submit" kind="primary" disabled={create.busy}>
            {t("create")}
          </Button>
        </div>
        <Failure message={create.error} />
      </form>
      {created && (
        <div className="mt-4 flex flex-col gap-2">
          <Note>{t("created")}</Note>
          <Copyable value={created.code} />
          {created.share_url && (
            <>
              <Note>{t("share")}</Note>
              <Copyable value={created.share_url} />
            </>
          )}
        </div>
      )}
    </Card>
  );
}

function InviteRow({ invite, reload }: { invite: Invite; reload: () => void }) {
  const t = useTranslations("admin.invites");
  const time = useTimeFormat();
  const revoke = useAction(useCallback(() => revokeInvite(invite.invite_id), [invite.invite_id]), reload);
  return (
    <li className="flex flex-col gap-1 py-3">
      <div className="flex items-baseline justify-between gap-3">
        <span>{invite.note || "—"}</span>
        <span className="text-sm text-muted">{t(`status.${invite.status}`)}</span>
      </div>
      <Note>
        {invite.used_by_name ? t("usedBy", { name: invite.used_by_name }) : t("expires", { time: time.dateTime(invite.expires_at) })}
      </Note>
      {invite.status === "unused" && (
        <div>
          <Button kind="danger" disabled={revoke.busy} onClick={() => void revoke.act()}>
            {t("revoke")}
          </Button>
        </div>
      )}
      <Failure message={revoke.error} />
    </li>
  );
}

export default function InvitesPage() {
  const t = useTranslations("admin.invites");
  const { data, error, loading, reload } = useApi(loadInvites);
  return (
    <Page title={t("title")} back="/me">
      <CreateForm reload={reload} />
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <Card>
          {data.length === 0 ? (
            <Note>{t("empty")}</Note>
          ) : (
            <ul className="divide-y divide-border">
              {data.map((invite) => (
                <InviteRow key={invite.invite_id} invite={invite} reload={reload} />
              ))}
            </ul>
          )}
        </Card>
      )}
    </Page>
  );
}
