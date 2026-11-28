"use client";

// 终端：这个账号的终端（在不在线、版本、要不要升级、从哪里连的、最近连接）、改名、移除；添加 = 输入终端上显示的配对码。
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import type { Terminal } from "@/api/generated/model";
import { listTerminals, pairTerminal, removeTerminal, renameTerminal } from "@/api/generated/terminals/terminals";
import { Button, Card, Failure, Loading, Note, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useAction, useApi } from "@/lib/useApi";

const loadTerminals = () => listTerminals();

function TerminalRow({ terminal, reload }: { terminal: Terminal; reload: () => void }) {
  const t = useTranslations("terminals");
  const tc = useTranslations("common");
  const time = useTimeFormat();
  const [name, setName] = useState<string | null>(null); // 改名中时是正在填的名字
  const [removing, setRemoving] = useState(false);
  const rename = useAction(useCallback((value: string) => renameTerminal(terminal.terminal_id, { name: value }), [terminal.terminal_id]), reload);
  const remove = useAction(useCallback(() => removeTerminal(terminal.terminal_id), [terminal.terminal_id]), reload);
  return (
    <li className="flex flex-col gap-2 py-3">
      <div className="flex items-center justify-between gap-3">
        <span className="font-medium">{terminal.name}</span>
        <span className={terminal.online ? "text-sm text-green-600" : "text-sm text-muted"}>{terminal.online ? t("online") : t("offline")}</span>
      </div>
      <Note>
        {t("version")} {terminal.version} · {t(`network.${terminal.network}`)} · {t("lastSeen")} {time.relative(terminal.last_seen_at)}
      </Note>
      {terminal.needs_upgrade && terminal.upgrade_note && <p className="text-sm text-amber-600">{terminal.upgrade_note}</p>}
      {name !== null ? (
        <form
          className="flex gap-2"
          onSubmit={async (event) => {
            event.preventDefault();
            if (await rename.act(name)) setName(null);
          }}
        >
          <input className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-3 py-2" value={name} onChange={(e) => setName(e.target.value)} />
          <Button type="submit" kind="primary" disabled={rename.busy}>
            {tc("save")}
          </Button>
          <Button onClick={() => setName(null)}>{tc("cancel")}</Button>
        </form>
      ) : removing ? (
        <div className="flex flex-col gap-2">
          <p className="text-sm">{t("removeConfirm")}</p>
          <div className="flex gap-2">
            <Button kind="danger" disabled={remove.busy} onClick={() => void remove.act()}>
              {tc("confirm")}
            </Button>
            <Button onClick={() => setRemoving(false)}>{tc("cancel")}</Button>
          </div>
        </div>
      ) : (
        <div className="flex gap-2">
          <Button onClick={() => setName(terminal.name)}>{t("rename")}</Button>
          <Button kind="danger" onClick={() => setRemoving(true)}>
            {t("remove")}
          </Button>
        </div>
      )}
      <Failure message={rename.error ?? remove.error} />
    </li>
  );
}

function PairForm({ reload }: { reload: () => void }) {
  const t = useTranslations("terminals");
  const [code, setCode] = useState("");
  const [paired, setPaired] = useState<string | null>(null);
  const pair = useAction(
    useCallback(async (value: string) => setPaired((await pairTerminal({ code: value })).terminal.name), []),
    reload,
  );
  return (
    <Card title={t("pairTitle")}>
      <Note>{t("pairHint")}</Note>
      <form
        className="mt-3 flex gap-2"
        onSubmit={async (event) => {
          event.preventDefault();
          if (await pair.act(code.trim())) setCode("");
        }}
      >
        <input
          className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-3 py-2 uppercase tracking-widest"
          placeholder={t("pairCode")}
          value={code}
          onChange={(e) => setCode(e.target.value)}
          autoComplete="off"
        />
        <Button type="submit" kind="primary" disabled={pair.busy || code.trim() === ""}>
          {t("pair")}
        </Button>
      </form>
      {paired && <p className="mt-2 text-sm text-green-600">{t("paired", { name: paired })}</p>}
      <Failure message={pair.error} />
    </Card>
  );
}

export default function TerminalsPage() {
  const t = useTranslations("terminals");
  const { data, error, loading, reload } = useApi(loadTerminals);
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
                <TerminalRow key={terminal.terminal_id} terminal={terminal} reload={reload} />
              ))}
            </ul>
          )}
        </Card>
      )}
      <PairForm reload={reload} />
    </Page>
  );
}
