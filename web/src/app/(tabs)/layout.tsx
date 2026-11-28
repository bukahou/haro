"use client";

// 登录后的四个标签页共用的外壳：没登录的回登录页（读回令牌之后才判断），底部是标签。
import { useRouter } from "next/navigation";
import { useEffect } from "react";

import { useSignedIn } from "@/auth/useSignedIn";
import { TabBar } from "@/components/TabBar";

export default function TabsLayout({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const { hydrated, signedIn } = useSignedIn();
  useEffect(() => {
    if (hydrated && !signedIn) {
      router.replace("/login");
    }
  }, [hydrated, signedIn, router]);
  if (!hydrated || !signedIn) {
    return null;
  }
  return (
    <>
      <main className="mx-auto max-w-3xl px-4 pt-6 pb-24">{children}</main>
      <TabBar />
    </>
  );
}
