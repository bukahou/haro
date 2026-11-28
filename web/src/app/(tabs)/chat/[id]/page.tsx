"use client";

// 一条对话（编号在地址里）。
import { useParams } from "next/navigation";

import { Thread } from "@/components/chat/Thread";

export default function ConversationPage() {
  const { id } = useParams<{ id: string }>();
  return (
    <div className="chat solo">
      <Thread key={id} conversationId={id} />
    </div>
  );
}
