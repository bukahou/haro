"use client";

// 长期记忆：haro 记下的事（常驻的每轮都带上，别的需要时按意思找）。可以忘掉、找回；忘掉的单独一节。
import { useTranslations } from "next-intl";
import { useState } from "react";

import { forgetNote, listNotes, restoreNote } from "@/api/generated/memory/memory";
import type { Note } from "@/api/generated/model";
import { showToast } from "@/components/overlay";
import { Button, Empty, Failure, Group, Loading, Page, SecTitle, Tag } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

const loadNotes = () => listNotes();

function NoteRow({ note, reload }: { note: Note; reload: () => void }) {
  const t = useTranslations("memory");
  const time = useTimeFormat();
  const [busy, setBusy] = useState(false);
  const forgotten = note.forgotten_at !== null;
  const act = async () => {
    setBusy(true);
    try {
      if (forgotten) {
        await restoreNote(note.note_id);
        showToast(t("restored"));
      } else {
        await forgetNote(note.note_id);
        showToast(t("forgotten"));
      }
      reload();
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="row" style={{ alignItems: "flex-start", cursor: "default" }}>
      <div className="grow">
        <div className={forgotten ? "text-muted line-through" : undefined}>{note.content}</div>
        <div className="wrap mt-1.5 items-center">
          {note.resident && <Tag tone="accent">{t("resident")}</Tag>}
          <span className="cap">{time.dateTime(note.created_at)}</span>
        </div>
      </div>
      <Button small kind={forgotten ? "outline" : "plain"} disabled={busy} onClick={() => void act()}>
        {forgotten ? t("restore") : t("forget")}
      </Button>
    </div>
  );
}

export default function MemoryPage() {
  const t = useTranslations("memory");
  const { data, error, loading, reload } = useApi(loadNotes);
  return (
    <Page title={t("title")} back="/me" sub={t("hint")}>
      {loading && <Loading />}
      <Failure message={error} />
      {data && (
        <>
          <p className="sub mb-2.5">{t("count", { count: data.total })}</p>
          {data.notes.length ? (
            <Group>
              {data.notes.map((note) => (
                <NoteRow key={note.note_id} note={note} reload={reload} />
              ))}
            </Group>
          ) : (
            <Empty>{t("empty")}</Empty>
          )}
          {data.forgotten.length > 0 && (
            <>
              <SecTitle>{t("forgottenSection")}</SecTitle>
              <Group>
                {data.forgotten.map((note) => (
                  <NoteRow key={note.note_id} note={note} reload={reload} />
                ))}
              </Group>
            </>
          )}
        </>
      )}
    </Page>
  );
}
