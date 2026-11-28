"use client";

// 背景记录池：后台从对话里整理的近况，每轮带给 haro（无痕对话不算）。可以整个关掉、改一条、删一条；上限和已用的字数由后端给。
import { Pencil, Trash2 } from "lucide-react";
import { useTranslations } from "next-intl";
import { useCallback, useState } from "react";

import { deleteBackstoryItem, editBackstoryItem, getBackstory, updateBackstory } from "@/api/generated/memory/memory";
import type { BackstoryItem } from "@/api/generated/model";
import { confirmDialog, showToast } from "@/components/overlay";
import { Banner, Button, Empty, Failure, Group, IconButton, Loading, Page, Tag, Toggle } from "@/components/ui";
import { useApi } from "@/lib/useApi";

const loadBackstory = () => getBackstory();

function ItemRow({ item, reload }: { item: BackstoryItem; reload: () => void }) {
  const t = useTranslations();
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  if (draft !== null) {
    return (
      <div className="row" style={{ alignItems: "flex-start", cursor: "default" }}>
        <div className="grow stack" style={{ gap: 8 }}>
          <textarea className="input" style={{ minHeight: 80 }} value={draft} onChange={(event) => setDraft(event.target.value)} aria-label={t("backstory.item")} />
          <div className="hstack">
            <Button
              small
              kind="solid"
              disabled={busy || !draft.trim()}
              onClick={async () => {
                setBusy(true);
                try {
                  await editBackstoryItem(item.item_id, { text: draft.trim() });
                  setDraft(null);
                  showToast(t("common.saved"));
                  reload();
                } finally {
                  setBusy(false);
                }
              }}
            >
              {t("common.save")}
            </Button>
            <Button small kind="plain" onClick={() => setDraft(null)}>
              {t("common.cancel")}
            </Button>
          </div>
        </div>
      </div>
    );
  }
  return (
    <div className="row" style={{ alignItems: "flex-start", cursor: "default" }}>
      <div className="grow">
        <div>{item.text}</div>
        {item.edited && (
          <div className="mt-1.5">
            <Tag>{t("backstory.edited")}</Tag>
          </div>
        )}
      </div>
      <IconButton icon={Pencil} label={t("common.edit")} muted size={18} onClick={() => setDraft(item.text)} />
      <IconButton
        icon={Trash2}
        label={t("common.delete")}
        muted
        size={18}
        onClick={() =>
          confirmDialog({
            title: t("backstory.deleteTitle"),
            body: t("backstory.deleteBody"),
            confirm: t("common.delete"),
            cancel: t("common.cancel"),
            run: async () => {
              await deleteBackstoryItem(item.item_id);
              reload();
            },
          })
        }
      />
    </div>
  );
}

export default function BackstoryPage() {
  const t = useTranslations("backstory");
  const { data, error, loading, reload } = useApi(loadBackstory);
  const [toggling, setToggling] = useState(false);
  const toggle = useCallback(
    async (enabled: boolean) => {
      setToggling(true);
      try {
        await updateBackstory({ enabled });
        showToast(enabled ? t("turnedOn") : t("turnedOff"));
        reload();
      } finally {
        setToggling(false);
      }
    },
    [reload, t],
  );
  return (
    <Page title={t("title")} back="/chat" sub={t("hint")} actions={data && <Toggle on={data.enabled} onChange={(on) => void toggle(on)} label={t("enabled")} disabled={toggling} />}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          {!data.enabled && (
            <div className="mb-3">
              <Banner>{t("off")}</Banner>
            </div>
          )}
          {data.items.length ? (
            <Group>
              {data.items.map((item) => (
                <ItemRow key={item.item_id} item={item} reload={reload} />
              ))}
            </Group>
          ) : (
            <Empty>{t("empty")}</Empty>
          )}
          <p className="cap mt-2.5">{t("usage", { used: data.used_chars, max: data.max_chars })}</p>
        </>
      )}
    </Page>
  );
}
