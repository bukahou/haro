"use client";

// 要带令牌取的图片（小票、账目的原图）：先取成 Blob 再给 <img>，离开时释放。
import { ImageIcon } from "lucide-react";
import { useEffect, useState } from "react";

export function BlobImage({ load, alt, className }: { load: () => Promise<Blob>; alt: string; className?: string }) {
  const [url, setURL] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let alive = true;
    let made: string | null = null;
    load().then(
      (blob) => {
        if (!alive) return;
        made = URL.createObjectURL(blob);
        setURL(made);
      },
      () => alive && setFailed(true),
    );
    return () => {
      alive = false;
      if (made) URL.revokeObjectURL(made);
    };
  }, [load]);
  return (
    <div className={className}>
      {/* eslint-disable-next-line @next/next/no-img-element -- 带令牌取回的图片（blob: 地址），不经 next/image */}
      {url ? <img src={url} alt={alt} /> : <ImageIcon size={28} aria-hidden className={failed ? "opacity-40" : undefined} />}
    </div>
  );
}
