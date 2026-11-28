"use client";

// 管理员：邀请码。生成（备注、多少小时后过期，不填用服务端的默认）后码和登录链接只显示这一次；
// 列表看状态（没用 / 用过 / 过期 / 撤销）、撤销没用的。
import { Copy } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import { createInvite, listInvites, revokeInvite } from "@/api/generated/admin/admin";
import type { CreatedInvite, InviteStatus } from "@/api/generated/model";
import { problemMessage } from "@/api/problem";
import { confirmDialog, showToast } from "@/components/overlay";
import { Button, Card, Empty, Failure, Field, Group, Loading, Page, SecTitle, Tag } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadInvites = () => listInvites();
const statusTone: Record<InviteStatus, "accent" | "neutral" | "danger"> = { unused: "accent", used: "neutral", expired: "neutral", revoked: "danger" };

function Copyable({ value, large }: { value: string; large?: boolean }) {
  const t = useTranslations("common");
  return (
    <div className="hstack w-full">
      <code className={large ? "num grow break-all text-xl tracking-widest" : "cap grow break-all"}>{value}</code>
      <Button small icon={Copy} onClick={() => void navigator.clipboard.writeText(value).then(() => showToast(t("copied")), () => showToast(t("copyFailed")))}>
        {t("copy")}
      </Button>
    </div>
  );
}

function CreateForm({ reload }: { reload: () => void }) {
  const t = useTranslations("admin.invites");
  const [note, setNote] = useState("");
  const [hours, setHours] = useState("");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<CreatedInvite | null>(null);
  const create = async () => {
    setBusy(true);
    try {
      setCreated(await createInvite({ note: note.trim() || undefined, expires_in_hours: hours ? Number(hours) : undefined }));
      setNote("");
      setHours("");
      reload();
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card title={t("create")}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void create();
        }}
      >
        <div className="form-grid">
          <Field label={t("note")}>
            <input className="input" value={note} maxLength={200} placeholder={t("notePlaceholder")} onChange={(event) => setNote(event.target.value)} />
          </Field>
          <Field label={t("hours")}>
            <input className="input num" type="number" min={1} max={720} value={hours} placeholder={t("hoursPlaceholder")} onChange={(event) => setHours(event.target.value)} />
          </Field>
        </div>
        <div className="actions">
          <Button kind="solid" type="submit" disabled={busy}>
            {t("create")}
          </Button>
        </div>
      </form>
      {created && (
        <div className="banner a mt-3.5 flex-col" style={{ gap: 10 }}>
          <div className="b">{t("created")}</div>
          <Copyable value={created.code} large />
          {created.share_url && (
            <>
              <div className="cap">{t("share")}</div>
              <Copyable value={created.share_url} />
            </>
          )}
        </div>
      )}
    </Card>
  );
}

export default function InvitesPage() {
  const t = useTranslations();
  const time = useTimeFormat();
  const { data, error, loading, reload } = useApi(loadInvites);
  return (
    <Page title={t("admin.invites.title")} back="/me" sub={t("admin.invites.hint")}>
      <CreateForm reload={reload} />
      <SecTitle>{t("admin.invites.sent")}</SecTitle>
      {loading && <Loading />}
      <Failure message={error} />
      {data &&
        (data.length ? (
          <Group>
            {data.map((invite) => (
              <div key={invite.invite_id} className="row" style={{ cursor: "default" }}>
                <span className="grow">
                  <span className="block">{invite.note || t("admin.invites.noNote")}</span>
                  <span className="cap">
                    {invite.used_by_name ? t("admin.invites.usedBy", { name: invite.used_by_name }) : t("admin.invites.expires", { time: time.dateTime(invite.expires_at) })}
                  </span>
                </span>
                <Tag tone={statusTone[invite.status]}>{t(`admin.invites.status.${invite.status}`)}</Tag>
                {invite.status === "unused" && (
                  <Button
                    small
                    kind="danger"
                    onClick={() =>
                      confirmDialog({
                        title: t("admin.invites.revokeTitle"),
                        body: t("admin.invites.revokeBody"),
                        confirm: t("admin.invites.revoke"),
                        cancel: t("common.cancel"),
                        run: async () => {
                          await revokeInvite(invite.invite_id);
                          showToast(t("admin.invites.revoked"));
                          reload();
                        },
                      })
                    }
                  >
                    {t("admin.invites.revoke")}
                  </Button>
                )}
              </div>
            ))}
          </Group>
        ) : (
          <Empty>{t("admin.invites.empty")}</Empty>
        ))}
    </Page>
  );
}
