"use client";

// 对话里的图片：要带令牌取（/attachments/{id}），所以先取成 Blob 再给 <img>。同一张只取一次（这一页开着期间）。
import { ImageIcon } from "lucide-react";
import { useEffect, useState } from "react";

import { getAttachment } from "@/api/generated/conversations/conversations";

const cache = new Map<string, Promise<string>>();

function urlOf(attachmentId: string): Promise<string> {
  let url = cache.get(attachmentId);
  if (!url) {
    url = getAttachment(attachmentId).then((blob) => URL.createObjectURL(blob));
    url.catch(() => cache.delete(attachmentId)); // 取失败的下次再试
    cache.set(attachmentId, url);
  }
  return url;
}

export function AttachmentImage({ attachmentId, alt }: { attachmentId: string; alt: string }) {
  const [url, setURL] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    urlOf(attachmentId).then(
      (value) => alive && setURL(value),
      () => undefined,
    );
    return () => {
      alive = false;
    };
  }, [attachmentId]);
  return (
    <div className="me-img">
      {/* eslint-disable-next-line @next/next/no-img-element -- 带令牌取回的图片（blob: 地址），不经 next/image */}
      {url ? <img src={url} alt={alt} /> : <ImageIcon size={28} aria-hidden />}
    </div>
  );
}
