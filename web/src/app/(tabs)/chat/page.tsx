"use client";

// 聊天标签。宽屏：对话列表在侧栏里，这里就是新对话；窄屏：对话列表（语音那条在最前），右上角新建、背景记录池。
import { BookOpen, Plus, Search } from "lucide-react";
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { ConversationList } from "@/components/chat/ConversationList";
import { NewConversation } from "@/components/chat/NewConversation";
import { IconButton } from "@/components/ui";
import { useWide } from "@/lib/useWide";

function MobileList() {
  const t = useTranslations("chat");
  const router = useRouter();
  const [filter, setFilter] = useState("");
  return (
    <aside className="chat-list">
      <div className="head">
        <h1>{t("title")}</h1>
        <IconButton icon={BookOpen} label={t("backstory")} href="/chat/backstory" />
        <IconButton icon={Plus} label={t("new")} href="/chat/new" />
      </div>
      <label className="search">
        <Search size={18} aria-hidden />
        <input
          placeholder={t("searchPlaceholder")}
          aria-label={t("searchPlaceholder")}
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.nativeEvent.isComposing && filter.trim()) {
              router.push(`/chat/search?${new URLSearchParams({ q: filter.trim() }).toString()}`);
            }
          }}
        />
      </label>
      <div className="conv-items">
        <ConversationList activeId={null} filter={filter} />
      </div>
    </aside>
  );
}

export default function ChatPage() {
  return useWide() ? <NewConversation /> : <MobileList />;
}
