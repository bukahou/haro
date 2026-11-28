"use client";

// 首页：登录了去聊天，没登录去登录页。
import { useRouter } from "next/navigation";
import { useEffect } from "react";

import { useSignedIn } from "@/auth/useSignedIn";

export default function Home() {
  const router = useRouter();
  const { hydrated, signedIn } = useSignedIn();
  useEffect(() => {
    if (hydrated) {
      router.replace(signedIn ? "/chat" : "/login");
    }
  }, [hydrated, signedIn, router]);
  return null;
}
