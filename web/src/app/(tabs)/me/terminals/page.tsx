"use client";

// 终端：这个账号的终端（在不在线、版本、要不要升级、从哪里连的、最近连接）、改名、移除。添加在单独一页（输配对码）。
import clsx from "clsx";
import { MonitorSmartphone, Pencil, Plus, Trash2 } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import type { Terminal } from "@/api/generated/model";
import { listTerminals, removeTerminal, renameTerminal } from "@/api/generated/terminals/terminals";
import { problemMessage } from "@/api/problem";
import { confirmDialog, showToast } from "@/components/overlay";
import { Banner, Button, Empty, Failure, Group, IconButton, Loading, Page, Tag } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";
import { useClientConfig } from "@/stores/clientConfig";

const loadTerminals = () => listTerminals();

function TerminalRow({ terminal, reload }: { terminal: Terminal; reload: () => void }) {
  const t = useTranslations();
  const time = useTimeFormat();
  const maxName = useClientConfig((state) => state.config?.limits.terminal_name_max_chars);
  const [name, setName] = useState<string | null>(null); // 改名中时是正在填的名字
  const [busy, setBusy] = useState(false);
  const saveName = async () => {
    if (name === null || !name.trim()) return;
    setBusy(true);
    try {
      await renameTerminal(terminal.terminal_id, { name: name.trim() });
      setName(null);
      showToast(t("common.saved"));
      reload();
    } catch (failure) {
      showToast(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="row" style={{ alignItems: "flex-start", cursor: "default" }}>
      <span className={clsx("ico", !terminal.online && "n")}>
        <MonitorSmartphone size={18} aria-hidden />
      </span>
      <div className="grow">
        {name !== null ? (
          <form
            className="hstack"
            onSubmit={(event) => {
              event.preventDefault();
              void saveName();
            }}
          >
            <input className="input" value={name} maxLength={maxName} autoFocus onChange={(event) => setName(event.target.value)} aria-label={t("terminals.rename")} />
            <Button small kind="solid" type="submit" disabled={busy || !name.trim()}>
              {t("common.save")}
            </Button>
            <Button small kind="plain" onClick={() => setName(null)}>
              {t("common.cancel")}
            </Button>
          </form>
        ) : (
          <div className="hstack">
            <span className="b">{terminal.name}</span>
            <Tag tone={terminal.online ? "accent" : "neutral"}>{terminal.online ? t("terminals.online") : t("terminals.offline")}</Tag>
          </div>
        )}
        <div className="cap num mt-0.5">
          {terminal.version} · {t(`terminals.network.${terminal.network}`)} · {t("terminals.lastSeen")} {time.relative(terminal.last_seen_at)}
        </div>
        {terminal.needs_upgrade && terminal.upgrade_note && (
          <div className="mt-2">
            <Banner>{terminal.upgrade_note}</Banner>
          </div>
        )}
      </div>
      {name === null && (
        <>
          <IconButton icon={Pencil} label={t("terminals.rename")} muted size={18} onClick={() => setName(terminal.name)} />
          <IconButton
            icon={Trash2}
            label={t("terminals.remove")}
            muted
            size={18}
            onClick={() =>
              confirmDialog({
                title: t("terminals.removeTitle", { name: terminal.name }),
                body: t("terminals.removeConfirm"),
                confirm: t("terminals.remove"),
                cancel: t("common.cancel"),
                run: async () => {
                  await removeTerminal(terminal.terminal_id);
                  showToast(t("terminals.removed"));
                  reload();
                },
              })
            }
          />
        </>
      )}
    </div>
  );
}

export default function TerminalsPage() {
  const t = useTranslations("terminals");
  const { data, error, loading, reload } = useApi(loadTerminals);
  return (
    <Page
      title={t("title")}
      back="/me"
      actions={
        <Button small kind="solid" href="/me/terminals/add" icon={Plus}>
          {t("pairTitle")}
        </Button>
      }
    >
      {loading && <Loading />}
      <Failure message={error} />
      {data &&
        (data.length ? (
          <Group>
            {data.map((terminal) => (
              <TerminalRow key={terminal.terminal_id} terminal={terminal} reload={reload} />
            ))}
          </Group>
        ) : (
          <Empty icon={MonitorSmartphone}>{t("empty")}</Empty>
        ))}
    </Page>
  );
}
