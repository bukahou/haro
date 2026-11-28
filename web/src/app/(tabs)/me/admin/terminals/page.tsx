"use client";

// 管理员：等批准的新终端（终端报的名字、编号、从哪里连的、版本）：批准 = 归一个新的匿名账号；拒绝。
import { useTranslations } from "next-intl";
import { useCallback } from "react";

import { approveTerminal, listPendingTerminals, rejectTerminal } from "@/api/generated/admin/admin";
import type { PendingTerminal } from "@/api/generated/model";
import { Button, Card, Failure, Loading, Note, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useAction, useApi } from "@/lib/useApi";

const loadPending = () => listPendingTerminals();

function PendingRow({ terminal, reload }: { terminal: PendingTerminal; reload: () => void }) {
  const t = useTranslations("admin.terminals");
  const tn = useTranslations("terminals");
  const time = useTimeFormat();
  const approve = useAction(useCallback(() => approveTerminal(terminal.terminal_id), [terminal.terminal_id]), reload);
  const reject = useAction(useCallback(() => rejectTerminal(terminal.terminal_id), [terminal.terminal_id]), reload);
  return (
    <li className="flex flex-col gap-2 py-3">
      <div className="flex items-baseline justify-between gap-3">
        <span className="font-medium">{terminal.reported_name}</span>
        <span className="font-mono text-sm text-muted">
          {t("shortId")} {terminal.short_id}
        </span>
      </div>
      <Note>
        {t("requested")} {time.dateTime(terminal.requested_at)} · {tn(`network.${terminal.network}`)} · {tn("version")} {terminal.version}
      </Note>
      <div className="flex gap-2">
        <Button kind="primary" disabled={approve.busy || reject.busy} onClick={() => void approve.act()}>
          {t("approve")}
        </Button>
        <Button kind="danger" disabled={approve.busy || reject.busy} onClick={() => void reject.act()}>
          {t("reject")}
        </Button>
      </div>
      <Failure message={approve.error ?? reject.error} />
    </li>
  );
}

export default function PendingTerminalsPage() {
  const t = useTranslations("admin.terminals");
  const { data, error, loading, reload } = useApi(loadPending);
  return (
    <Page title={t("title")} back="/me">
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <Card>
          {data.length === 0 ? (
            <Note>{t("empty")}</Note>
          ) : (
            <ul className="divide-y divide-border">
              {data.map((terminal) => (
                <PendingRow key={terminal.terminal_id} terminal={terminal} reload={reload} />
              ))}
            </ul>
          )}
        </Card>
      )}
    </Page>
  );
}
