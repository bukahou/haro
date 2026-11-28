"use client";

// 聊天里的卡片：派出去的后台调查（进度和结论从调查的接口读）、要主人确认的操作（点确认 / 取消交给后端）。
import { AlertTriangle, Check, FlaskConical } from "lucide-react";
import { useTranslations } from "next-intl";
import { useEffect, useState } from "react";

import { getResearchTask } from "@/api/generated/memory/memory";
import type { ConfirmationCard as Confirmation, ResearchCard as Research, ResearchTask } from "@/api/generated/model";
import { Markdown } from "@/components/chat/Markdown";
import { researchTone } from "@/components/me/researchTone";
import { Button, Spinner, Tag } from "@/components/ui";

// 调查还在查时，多久再读一次进度
const researchPollMillis = 15_000;

export function ResearchCardView({ research }: { research: Research }) {
  const t = useTranslations("chat.research");
  const [task, setTask] = useState<ResearchTask | null>(null);
  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () =>
      getResearchTask(research.task_id).then(
        (latest) => {
          if (!alive) return;
          setTask(latest);
          if (latest.status === "queued" || latest.status === "running") timer = setTimeout(read, researchPollMillis);
        },
        () => undefined, // 读不到时卡片只显示问题
      );
    void read();
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [research.task_id]);
  const running = !task || task.status === "queued" || task.status === "running";
  return (
    <div className="res-card">
      <div className="between">
        <span className="hstack sub">
          <FlaskConical size={16} aria-hidden />
          {t("title")}
        </span>
        {task ? (
          <Tag tone={researchTone[task.status]}>
            {running && <Spinner />}
            {task.status_label}
          </Tag>
        ) : (
          <Spinner />
        )}
      </div>
      <div className="b">{research.question}</div>
      {task?.conclusion && (
        <div className="ai-msg excerpt two">
          <Markdown text={task.conclusion} />
        </div>
      )}
      {task?.stop_reason && !task.conclusion && <div className="cap">{task.stop_reason}</div>}
      {running && <div className="cap">{t("running")}</div>}
      <div>
        <Button small href={`/me/research/${research.task_id}`}>
          {task?.conclusion ? t("open") : t("details")}
        </Button>
      </div>
    </div>
  );
}

export function ConfirmationCardView({ confirmation, busy, onDecide }: { confirmation: Confirmation; busy: boolean; onDecide: (confirm: boolean) => void }) {
  const t = useTranslations("chat.confirmation");
  const expiresAt = Date.parse(confirmation.expires_at);
  const [now, setNow] = useState(() => Date.now());
  const expired = now >= expiresAt;
  const open = confirmation.open && !expired;
  useEffect(() => {
    if (!confirmation.open || Date.now() >= expiresAt) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [confirmation.open, expiresAt]);
  const seconds = Math.max(0, Math.ceil((expiresAt - now) / 1000));
  return (
    <div className="confirm-card">
      <div className="hstack">
        <span className={open ? "ico d" : "ico n"}>{open ? <AlertTriangle size={18} aria-hidden /> : <Check size={18} aria-hidden />}</span>
        <div className="grow">
          <div className="b">{confirmation.label}</div>
          <div className="sub">{open ? t("needed") : expired && confirmation.open ? t("expired") : t("closed")}</div>
        </div>
      </div>
      {open && (
        <>
          <div className="hstack" style={{ gap: 10 }}>
            <Button small disabled={busy} onClick={() => onDecide(false)}>
              {t("cancel")}
            </Button>
            <Button small kind="danger" disabled={busy} onClick={() => onDecide(true)}>
              {t("confirm")}
            </Button>
          </div>
          <div className="cap num">{t("countdown", { seconds })}</div>
        </>
      )}
    </div>
  );
}

// ToolLine 是一个工具：在做时转圈，做完打勾；文字是后端给的说明
export function ToolLine({ label, done }: { label: string; done: boolean }) {
  return (
    <div className="tool">
      {done ? <Check size={16} className="acc" aria-hidden /> : <Spinner />}
      {label}
    </div>
  );
}
