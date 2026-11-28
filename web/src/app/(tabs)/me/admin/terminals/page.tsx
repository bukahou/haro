"use client";

// 管理员：等批准的新终端（终端报的名字、编号、从哪里连的、版本）。批准 = 归一个新的匿名账号；拒绝。
// 批准前对一下终端上显示的编号。
import { MonitorSmartphone } from "lucide-react";
import { useTranslations } from "next-intl";

import { approveTerminal, listPendingTerminals, rejectTerminal } from "@/api/generated/admin/admin";
import type { PendingTerminal } from "@/api/generated/model";
import { confirmDialog, showToast } from "@/components/overlay";
import { Button, Empty, Failure, Ico, Loading, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";
import { useAccount } from "@/stores/account";

const loadPending = () => listPendingTerminals();

function PendingCard({ terminal, reload }: { terminal: PendingTerminal; reload: () => void }) {
  const t = useTranslations();
  const time = useTimeFormat();
  const done = (text: string) => {
    showToast(text);
    reload();
    void useAccount.getState().refresh();
  };
  return (
    <div className="card m-0">
      <div className="between">
        <span className="hstack">
          <Ico icon={MonitorSmartphone} tone="neutral" />
          <span className="b">{terminal.reported_name}</span>
        </span>
        <span className="cap num">
          {t("admin.terminals.shortId")} {terminal.short_id}
        </span>
      </div>
      <p className="cap mt-1.5">
        {t("admin.terminals.requested")} {time.dateTime(terminal.requested_at)} · {t(`terminals.network.${terminal.network}`)} · {terminal.version}
      </p>
      <p className="sub mt-1.5">{t("admin.terminals.checkId", { id: terminal.short_id })}</p>
      <div className="actions">
        <Button
          small
          kind="solid"
          onClick={() =>
            confirmDialog({
              title: t("admin.terminals.approveTitle", { name: terminal.reported_name }),
              body: t("admin.terminals.approveBody", { id: terminal.short_id }),
              confirm: t("admin.terminals.approve"),
              cancel: t("common.cancel"),
              kind: "solid",
              run: async () => {
                await approveTerminal(terminal.terminal_id);
                done(t("admin.terminals.approved"));
              },
            })
          }
        >
          {t("admin.terminals.approve")}
        </Button>
        <Button
          small
          kind="danger"
          onClick={() =>
            confirmDialog({
              title: t("admin.terminals.rejectTitle", { name: terminal.reported_name }),
              body: t("admin.terminals.rejectBody"),
              confirm: t("admin.terminals.reject"),
              cancel: t("common.cancel"),
              run: async () => {
                await rejectTerminal(terminal.terminal_id);
                done(t("admin.terminals.rejected"));
              },
            })
          }
        >
          {t("admin.terminals.reject")}
        </Button>
      </div>
    </div>
  );
}

export default function PendingTerminalsPage() {
  const t = useTranslations("admin.terminals");
  const { data, error, loading, reload } = useApi(loadPending);
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data &&
        (data.length ? (
          <div className="stack">
            {data.map((terminal) => (
              <PendingCard key={terminal.terminal_id} terminal={terminal} reload={reload} />
            ))}
          </div>
        ) : (
          <Empty icon={MonitorSmartphone}>{t("empty")}</Empty>
        ))}
    </Page>
  );
}
