"use client";

// 搜以前的对话（后端按文字和按意思找，合在一起、新的在前）：一条结果 = 一轮，点了进那条对话。
import { Search } from "lucide-react";
import { useTranslations } from "next-intl";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useCallback, useState } from "react";

import { searchTurns } from "@/api/generated/conversations/conversations";
import { useConversationTitle } from "@/components/chat/ConversationList";
import { Markdown } from "@/components/chat/Markdown";
import { Empty, Failure, Group, Loading, Page } from "@/components/ui";
import { useTimeFormat } from "@/lib/format";
import { useApi } from "@/lib/useApi";

function Results({ q }: { q: string }) {
  const t = useTranslations("chat");
  const time = useTimeFormat();
  const titleOf = useConversationTitle();
  const { data, error, loading } = useApi(useCallback(() => searchTurns({ q }), [q]));
  if (loading) return <Loading />;
  if (error) return <Failure message={error} />;
  if (!data?.length) return <Empty icon={Search}>{t("searchNone", { q })}</Empty>;
  return (
    <Group>
      {data.map((found) => {
        const reply = found.turn.reply.filter((block) => block.type === "text").map((block) => block.text).join("\n\n");
        return (
          <Link key={`${found.conversation_id}:${found.turn.seq}`} className="row" href={`/chat/${found.conversation_id}`} style={{ alignItems: "flex-start" }}>
            <span className="grow">
              <span className="between">
                <span className="b ell">{titleOf({ kind: found.kind, title: found.title })}</span>
                <span className="cap">{time.short(found.turn.at)}</span>
              </span>
              {found.turn.said && <span className="block ell">{found.turn.said}</span>}
              {reply && (
                <span className="sub excerpt two">
                  <Markdown text={reply} />
                </span>
              )}
            </span>
          </Link>
        );
      })}
    </Group>
  );
}

function SearchPanel() {
  const t = useTranslations("chat");
  const router = useRouter();
  const q = useSearchParams().get("q")?.trim() ?? "";
  const [input, setInput] = useState(q);
  return (
    <Page title={t("searchTitle")} back="/chat">
      <label className="search mx-0 mb-4" style={{ background: "var(--card)" }}>
        <Search size={18} aria-hidden />
        <input
          placeholder={t("searchPlaceholder")}
          aria-label={t("searchPlaceholder")}
          value={input}
          onChange={(event) => setInput(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.nativeEvent.isComposing && input.trim()) {
              router.replace(`/chat/search?${new URLSearchParams({ q: input.trim() }).toString()}`);
            }
          }}
        />
      </label>
      {q && <Results key={q} q={q} />}
    </Page>
  );
}

export default function SearchPage() {
  return (
    <Suspense>
      <SearchPanel />
    </Suspense>
  );
}
