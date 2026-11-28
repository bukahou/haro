"use client";

// 对话列表（在侧栏里）：语音那条在最前（单独一节），其余新的在前，一行一条。
// filter 只筛已经读到的标题和最近一句（搜内容走 /chat/search，由后端搜）。
import clsx from "clsx";
import { BookOpen, MessageCircle, Mic, VenetianMask } from "lucide-react";
import { useTranslations } from "next-intl";
import Link from "next/link";

import type { Conversation } from "@/api/generated/model";
import { useTimeFormat } from "@/lib/format";
import { useConversations } from "@/stores/conversations";

// 对话的标题：后端给的；还没有标题时按种类写（语音那条的名字由 App 按语言写，见契约 Conversation.title）
export function useConversationTitle() {
  const t = useTranslations("chat");
  return (conversation: Pick<Conversation, "kind" | "title"> | undefined) =>
    conversation?.title ?? (conversation?.kind === "voice" ? t("voiceTitle") : t("untitled"));
}

export function ConversationList({ activeId, filter }: { activeId: string | null; filter: string }) {
  const t = useTranslations("chat");
  const time = useTimeFormat();
  const titleOf = useConversationTitle();
  const { list, error } = useConversations();
  if (error && !list) return <p className="cap px-2.5 py-3">{error}</p>;
  if (!list) return null;
  const needle = filter.trim().toLowerCase();
  const voice = list.filter((item) => item.kind === "voice");
  const chats = list.filter(
    (item) => item.kind !== "voice" && (!needle || `${titleOf(item)} ${item.last_text ?? ""}`.toLowerCase().includes(needle)),
  );
  const item = (conversation: Conversation) => {
    const Icon = conversation.kind === "voice" ? Mic : conversation.incognito ? VenetianMask : MessageCircle;
    return (
      <Link
        key={conversation.conversation_id}
        href={`/chat/${conversation.conversation_id}`}
        className={clsx("conv", conversation.conversation_id === activeId && "on")}
        aria-current={conversation.conversation_id === activeId ? "page" : undefined}
      >
        <span className={clsx("ico", conversation.kind !== "voice" && "n")}>
          <Icon size={15} aria-hidden />
        </span>
        <span className="grow">
          <span className="between">
            <span className="t ell">{titleOf(conversation)}</span>
            <span className="cap">{time.short(conversation.updated_at)}</span>
          </span>
        </span>
      </Link>
    );
  };
  return (
    <>
      {voice.length > 0 && <div className="conv-sec">{t("voiceSection")}</div>}
      {voice.map(item)}
      <div className="conv-sec between">
        {t("chatSection")}
        <Link className="sec-link" href="/chat/backstory">
          <BookOpen size={14} aria-hidden />
          {t("backstory")}
        </Link>
      </div>
      {chats.length ? chats.map(item) : <p className="sub px-2.5 py-3">{needle ? t("noMatch", { filter }) : t("noChats")}</p>}
    </>
  );
}
