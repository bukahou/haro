"use client";

// 一条对话：页头（返回、标题、更多）、几轮记录（往上翻读更早的）、正在答的那一轮、输入框。
// 数据都在 stores/chat.ts；这里只管显示和把主人的操作交过去。
import clsx from "clsx";
import { AlertTriangle, ArrowUp, Check, ChevronDown, ChevronRight, Copy, ImageIcon, MessageCircle, Mic, MoreHorizontal, Square, VenetianMask, X } from "lucide-react";
import { useTranslations } from "next-intl";
import { useRouter } from "next/navigation";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import { deleteConversation, restoreConversation } from "@/api/generated/conversations/conversations";
import type { ConfirmationCard, Conversation, ReplyBlock, ResearchCard, Turn } from "@/api/generated/model";
import { openDialog, showToast } from "@/components/overlay";
import { Banner, Button, Spinner, Tag } from "@/components/ui";
import { useChat, type LiveBlock, type LiveTurn } from "@/stores/chat";
import { useClientConfig } from "@/stores/clientConfig";
import { useConversations } from "@/stores/conversations";

import { MenuButton } from "@/components/shell/MenuButton";

import { AttachmentImage } from "./AttachmentImage";
import { ConfirmationCardView, ResearchCardView, ToolLine } from "./Cards";
import { useConversationTitle } from "./ConversationList";
import { Markdown } from "./Markdown";

// ---------- 一轮的显示 ----------

type Shown =
  | { type: "text"; text: string; streaming?: boolean }
  | { type: "tools"; tools: { key: string; label: string; done: boolean }[] }
  | { type: "research"; research: ResearchCard }
  | { type: "confirmation"; confirmation: ConfirmationCard };

// 记录里的回答 → 要显示的几块：连着的工具合成一组
function shownOfReply(blocks: ReplyBlock[]): Shown[] {
  const shown: Shown[] = [];
  blocks.forEach((block, index) => {
    const last = shown[shown.length - 1];
    if (block.type === "tool") {
      const tool = { key: String(index), label: block.label ?? block.tool ?? "", done: true };
      if (last?.type === "tools") last.tools.push(tool);
      else shown.push({ type: "tools", tools: [tool] });
    } else if (block.type === "text" && block.text) {
      shown.push({ type: "text", text: block.text });
    } else if (block.type === "research" && block.research) {
      shown.push({ type: "research", research: block.research });
    } else if (block.type === "confirmation" && block.confirmation) {
      shown.push({ type: "confirmation", confirmation: block.confirmation });
    }
  });
  return shown;
}

// 正在答的这一轮 → 要显示的几块（最后一段文字带着光标）
function shownOfLive(blocks: LiveBlock[], answering: boolean): Shown[] {
  const shown: Shown[] = [];
  blocks.forEach((block, index) => {
    const last = shown[shown.length - 1];
    if (block.type === "tool") {
      const tool = { key: block.callId || String(index), label: block.label, done: block.done };
      if (last?.type === "tools") last.tools.push(tool);
      else shown.push({ type: "tools", tools: [tool] });
    } else if (block.type === "text") {
      shown.push({ type: "text", text: block.text, streaming: answering && index === blocks.length - 1 });
    } else {
      shown.push(block);
    }
  });
  return shown;
}

// 一组工具：只有一个、或者还在做时一行一行列出来；都做完、不止一个时收成「用了 N 个工具」，点开看每一个
function ToolGroup({ tools }: { tools: { key: string; label: string; done: boolean }[] }) {
  const t = useTranslations("chat");
  const [open, setOpen] = useState(false);
  const folded = tools.length > 1 && tools.every((tool) => tool.done);
  if (!folded) {
    return (
      <div className="stack" style={{ gap: 6 }}>
        {tools.map((tool) => (
          <ToolLine key={tool.key} label={tool.label} done={tool.done} />
        ))}
      </div>
    );
  }
  return (
    <div>
      <button type="button" className="tool tool-fold" onClick={() => setOpen(!open)} aria-expanded={open}>
        <Check size={16} className="acc" aria-hidden />
        {t("usedTools", { count: tools.length })}
        {open ? <ChevronDown size={14} aria-hidden /> : <ChevronRight size={14} aria-hidden />}
      </button>
      {open && (
        <div className="stack ml-6 mt-1.5" style={{ gap: 4 }}>
          {tools.map((tool) => (
            <div key={tool.key} className="tool">
              {tool.label}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ShownBlocks({ shown, conversationId, busy }: { shown: Shown[]; conversationId: string; busy: boolean }) {
  const decide = useChat((store) => store.decide);
  return (
    <>
      {shown.map((block, index) => {
        switch (block.type) {
          case "tools":
            return <ToolGroup key={index} tools={block.tools} />;
          case "text":
            return (
              <div key={index} className={clsx("ai-msg", block.streaming && "streaming")}>
                <Markdown text={block.text} />
              </div>
            );
          case "research":
            return <ResearchCardView key={index} research={block.research} />;
          case "confirmation":
            return (
              <ConfirmationCardView
                key={index}
                confirmation={block.confirmation}
                busy={busy}
                onDecide={(confirm) => void decide(conversationId, block.confirmation.action_id, confirm)}
              />
            );
        }
      })}
    </>
  );
}

function CopyReply({ text }: { text: string }) {
  const t = useTranslations("chat");
  return (
    <div className="msg-acts">
      <button
        type="button"
        className="icon-btn mut"
        aria-label={t("copy")}
        title={t("copy")}
        onClick={() =>
          navigator.clipboard.writeText(text).then(
            () => showToast(t("copied")),
            () => showToast(t("copyFailed")),
          )
        }
      >
        <Copy size={18} />
      </button>
    </div>
  );
}

function TurnView({ turn, conversationId, busy }: { turn: Turn; conversationId: string; busy: boolean }) {
  const t = useTranslations("chat");
  const replyText = turn.reply.filter((block) => block.type === "text" && block.text).map((block) => block.text).join("\n\n");
  return (
    <>
      {turn.attachment && <AttachmentImage attachmentId={turn.attachment} alt={t("sentImage")} />}
      {turn.said && <div className="me-msg">{turn.said}</div>}
      <ShownBlocks shown={shownOfReply(turn.reply)} conversationId={conversationId} busy={busy} />
      {replyText && <CopyReply text={replyText} />}
    </>
  );
}

function LiveView({ live, conversationId, onResend, onDismiss, onReload }: { live: LiveTurn; conversationId: string; onResend: () => void; onDismiss: () => void; onReload: () => void }) {
  const t = useTranslations("chat");
  const said = live.request.kind === "message" ? live.request.text : t(live.request.confirm ? "card.confirm" : "card.cancel");
  return (
    <>
      {!live.settled && (
        <>
          {live.imageURL && (
            <div className="me-img">
              {/* eslint-disable-next-line @next/next/no-img-element -- 本机选的图片（blob: 地址），不经 next/image */}
              <img src={live.imageURL} alt={t("sentImage")} />
            </div>
          )}
          <div className="me-msg">{said}</div>
          {live.answering && live.blocks.length === 0 && !live.failure && (
            <div className="tool">
              <Spinner />
              {t("thinking")}
            </div>
          )}
          <ShownBlocks shown={shownOfLive(live.blocks, live.answering)} conversationId={conversationId} busy={live.answering} />
        </>
      )}
      {live.ending && !live.answering && <p className="cap">{t(`ending.${live.ending}`)}</p>}
      {live.failure && !live.answering && (
        <div className="max-w-[520px]">
          <Banner tone="danger" icon={AlertTriangle}>
            <div className="b">{live.notSent ? t("notSent") : t("failed")}</div>
            <div>{live.failure.kind === "cut" ? t("cut") : live.failure.message}</div>
            <div className="mt-2 flex gap-2">
              {live.notSent && (
                <Button small onClick={onResend}>
                  {t("resend")}
                </Button>
              )}
              {live.failure.kind === "cut" && (
                <Button small onClick={onReload}>
                  {t("reload")}
                </Button>
              )}
              <Button small kind="plain" onClick={onDismiss}>
                {t("dismiss")}
              </Button>
            </div>
          </Banner>
        </div>
      )}
    </>
  );
}

// ---------- 输入框 ----------

export function Composer({ busy, onSend, onStop, placeholder }: { busy: boolean; onSend: (text: string, image: File | null) => void; onStop: () => void; placeholder: string }) {
  const t = useTranslations("chat");
  const maxBytes = useClientConfig((state) => state.config?.limits.attachment_max_bytes);
  const [text, setText] = useState("");
  const [image, setImage] = useState<File | null>(null);
  const preview = useMemo(() => (image ? URL.createObjectURL(image) : null), [image]);
  const box = useRef<HTMLTextAreaElement>(null);

  useEffect(() => () => void (preview && URL.revokeObjectURL(preview)), [preview]);

  const pick = (file: File | null | undefined) => {
    if (!file) return;
    if (maxBytes && file.size > maxBytes) {
      showToast(t("imageTooLarge", { mb: Math.floor(maxBytes / 1024 / 1024) }));
      return;
    }
    setImage(file);
  };
  const grow = () => {
    const element = box.current;
    if (!element) return;
    element.style.height = "auto";
    element.style.height = `${Math.min(element.scrollHeight, 160)}px`;
    element.style.overflowY = element.scrollHeight > 160 ? "auto" : "hidden"; // 到了最高才出滚动条
  };
  const submit = () => {
    const value = text.trim();
    if (busy || !value) return; // 契约：一句必须有文字（图片是附在这句上的）
    onSend(value, image);
    setText("");
    setImage(null);
    requestAnimationFrame(grow);
  };

  return (
    <div className="composer">
      {image && preview && (
        <div className="attach-preview">
          <div className="thumb">
            {/* eslint-disable-next-line @next/next/no-img-element -- 本机选的图片（blob: 地址），不经 next/image */}
            <img src={preview} alt={t("pendingImage")} />
            <button type="button" className="x" onClick={() => setImage(null)} aria-label={t("removeImage")}>
              <X size={12} />
            </button>
          </div>
          <span className="cap">{t("imageHint")}</span>
        </div>
      )}
      <div className="composer-inner">
        <label className="round cursor-pointer" title={t("addImage")} aria-label={t("addImage")}>
          <ImageIcon size={20} />
          <input
            type="file"
            accept="image/jpeg,image/png,image/webp"
            hidden
            onChange={(event) => {
              pick(event.target.files?.[0]);
              event.target.value = "";
            }}
          />
        </label>
        <textarea
          ref={box}
          rows={1}
          value={text}
          placeholder={placeholder}
          aria-label={placeholder}
          onChange={(event) => {
            setText(event.target.value);
            grow();
          }}
          onPaste={(event) => {
            const file = [...event.clipboardData.files].find((item) => item.type.startsWith("image/"));
            if (file) {
              event.preventDefault();
              pick(file);
            }
          }}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
              event.preventDefault();
              submit();
            }
          }}
        />
        {busy ? (
          <button type="button" className="round solid" onClick={onStop} aria-label={t("stop")} title={t("stop")}>
            <Square size={16} fill="currentColor" />
          </button>
        ) : (
          <button type="button" className="round solid" onClick={submit} disabled={!text.trim()} aria-label={t("send")} title={t("send")}>
            <ArrowUp size={20} />
          </button>
        )}
      </div>
    </div>
  );
}

// ---------- 一条对话 ----------

function ThreadHead({ conversation, conversationId }: { conversation: Conversation | undefined; conversationId: string }) {
  const t = useTranslations();
  const router = useRouter();
  const titleOf = useConversationTitle();
  const upsert = useConversations((state) => state.upsert);
  const remove = useConversations((state) => state.remove);
  const voice = conversation?.kind === "voice";
  const Icon = voice ? Mic : conversation?.incognito ? VenetianMask : MessageCircle;
  const askDelete = () =>
    openDialog({
      title: t("chat.deleteTitle"),
      body: t("chat.deleteBody", { title: titleOf(conversation) }),
      buttons: [
        { label: t("common.cancel"), kind: "plain" },
        {
          label: t("common.delete"),
          kind: "danger",
          run: async () => {
            await deleteConversation(conversationId);
            remove(conversationId);
            router.replace("/chat");
            showToast(t("chat.deleted"), {
              label: t("chat.undo"),
              run: async () => {
                upsert(await restoreConversation(conversationId));
                router.push(`/chat/${conversationId}`);
              },
            });
          },
        },
      ],
    });
  return (
    <div className="thread-head">
      <MenuButton />
      <span className={clsx("ico", !voice && "n")} style={{ width: 30, height: 30 }}>
        <Icon size={16} aria-hidden />
      </span>
      <div className="title ell">
        {titleOf(conversation)} {conversation?.incognito && <Tag>{t("chat.incognito")}</Tag>}
      </div>
      {conversation && !voice && (
        <button type="button" className="icon-btn mut" onClick={askDelete} aria-label={t("chat.more")} title={t("chat.deleteTitle")}>
          <MoreHorizontal size={20} />
        </button>
      )}
    </div>
  );
}

export function Thread({ conversationId }: { conversationId: string }) {
  const t = useTranslations("chat");
  const conversation = useConversations((state) => state.list?.find((item) => item.conversation_id === conversationId));
  const state = useChat((store) => store.threads[conversationId]);
  const { load, loadOlder, send, retry, stop, dismiss } = useChat.getState();
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true); // 在底部时，新内容进来跟着滚到底
  const olderAnchor = useRef<number | null>(null);

  useEffect(() => {
    if (!useChat.getState().threads[conversationId]?.loaded) void load(conversationId);
  }, [conversationId, load]);

  const turns = state?.turns;
  const live = state?.live ?? null;
  useLayoutEffect(() => {
    const element = scroller.current;
    if (!element) return;
    if (olderAnchor.current !== null) {
      element.scrollTop = element.scrollHeight - olderAnchor.current; // 读了更早的：停在原来看的位置
      olderAnchor.current = null;
    } else if (pinned.current) {
      element.scrollTop = element.scrollHeight;
    }
  }, [turns, live]);

  const readOlder = () => {
    const element = scroller.current;
    if (element) olderAnchor.current = element.scrollHeight - element.scrollTop;
    void loadOlder(conversationId);
  };

  if (state?.missing) {
    return (
      <section className="thread">
        <div className="no-thread">
          <MessageCircle size={32} aria-hidden />
          <p>{t("missing")}</p>
          <Button small href="/chat">
            {t("backToList")}
          </Button>
        </div>
      </section>
    );
  }

  const voice = conversation?.kind === "voice";
  return (
    <section className="thread">
      <ThreadHead conversation={conversation} conversationId={conversationId} />
      {voice && (
        <div className="mx-5 mt-3">
          <Banner tone="accent" icon={Mic}>
            {t("voiceBanner")}
          </Banner>
        </div>
      )}
      <div
        className="msgs"
        ref={scroller}
        onScroll={(event) => {
          const element = event.currentTarget;
          pinned.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80;
        }}
      >
        <div className="msgs-inner">
          {state?.nextBefore != null && (
            <div className="self-center">
              <Button small kind="plain" onClick={readOlder} disabled={state.loadingOlder}>
                {state.loadingOlder ? <Spinner /> : null}
                {t("older")}
              </Button>
            </div>
          )}
          {state?.error && <Banner tone="danger">{state.error}</Banner>}
          {!state?.loaded && (
            <div className="stack" aria-busy="true">
              <div className="sk" style={{ width: "40%", alignSelf: "flex-end" }} />
              <div className="sk" style={{ width: "80%" }} />
              <div className="sk" style={{ width: "65%" }} />
            </div>
          )}
          {state?.loaded && !turns?.length && !live && <EmptyThread incognito={!!conversation?.incognito} />}
          {turns?.map((turn) => (
            <TurnView key={turn.seq} turn={turn} conversationId={conversationId} busy={!!live?.answering} />
          ))}
          {live && (
            <LiveView
              live={live}
              conversationId={conversationId}
              onResend={() => void retry(conversationId)}
              onDismiss={() => dismiss(conversationId)}
              onReload={() => {
                dismiss(conversationId);
                void load(conversationId);
              }}
            />
          )}
        </div>
      </div>
      <Composer
        busy={!!live?.answering}
        placeholder={t("placeholder")}
        onStop={() => void stop(conversationId)}
        onSend={(text, image) => {
          pinned.current = true;
          void send(conversationId, text, image);
        }}
      />
    </section>
  );
}

export function EmptyThread({ incognito }: { incognito: boolean }) {
  const t = useTranslations("chat");
  return (
    <div className="empty" style={{ margin: "auto" }}>
      <div className="big">{incognito ? <VenetianMask size={26} /> : <MessageCircle size={26} />}</div>
      <div className="b text-lg text-ink">{incognito ? t("incognitoTitle") : t("emptyTitle")}</div>
      <p className="sub mx-auto mt-1.5 max-w-xs">{incognito ? t("incognitoHint") : t("emptyHint")}</p>
    </div>
  );
}
