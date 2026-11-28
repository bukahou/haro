"use client";

// 聊天：打开就是新对话（像 ChatGPT / Claude）；以前的对话在侧栏里（窄屏从左边滑出来）。
import { NewConversation } from "@/components/chat/NewConversation";

export default function ChatPage() {
  return <NewConversation />;
}
