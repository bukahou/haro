"use client";

// 新对话：先只有输入框（像网页版的 Claude）；发出第一句时才在服务端建这条对话，然后地址换成它的编号。
// 无痕 = 不进背景记录池、长期记忆也查不到它（对话本身照常保存）。
import { MessageCircle, VenetianMask } from "lucide-react";
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { problemMessage } from "@/api/problem";
import { showToast } from "@/components/overlay";
import { Toggle } from "@/components/ui";
import { useChat } from "@/stores/chat";

import { Composer, EmptyThread } from "./Thread";

export function NewConversation() {
  const t = useTranslations("chat");
  const router = useRouter();
  const [incognito, setIncognito] = useState(false);
  const [busy, setBusy] = useState(false);
  return (
    <div className="chat solo">
      <section className="thread">
        <div className="thread-head">
          <span className="ico n" style={{ width: 30, height: 30 }}>
            {incognito ? <VenetianMask size={16} aria-hidden /> : <MessageCircle size={16} aria-hidden />}
          </span>
          <div className="title ell">{t("new")}</div>
          <label className="hstack sub">
            {t("incognito")}
            <Toggle on={incognito} onChange={setIncognito} label={t("incognito")} />
          </label>
        </div>
        <div className="msgs">
          <div className="msgs-inner" style={{ flex: 1 }}>
            <EmptyThread incognito={incognito} />
          </div>
        </div>
        <Composer
          busy={busy}
          placeholder={t("placeholder")}
          onStop={() => undefined}
          onSend={async (text, image) => {
            setBusy(true);
            try {
              const conversation = await useChat.getState().start(incognito);
              void useChat.getState().send(conversation.conversation_id, text, image);
              router.replace(`/chat/${conversation.conversation_id}`);
            } catch (failure) {
              showToast(problemMessage(failure));
              setBusy(false);
            }
          }}
        />
      </section>
    </div>
  );
}
