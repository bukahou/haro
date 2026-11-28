"use client";

// 登录后各页共用的外壳：没登录的回登录页（读回令牌之后才判断）。
import { useRouter } from "next/navigation";
import { useEffect } from "react";

import { useSignedIn } from "@/auth/useSignedIn";
import { AppShell } from "@/components/shell/AppShell";

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
  return <AppShell>{children}</AppShell>;
}
