import type { Metadata, Viewport } from "next";

import { IntlProvider, interfaceLocale } from "@/i18n/IntlProvider";

import "./globals.css";

export const metadata: Metadata = {
  title: "haro",
  description: "haro 的网页版",
  // 构建的源码版本（CI 经 Dockerfile 传入；本地是 dev）：线上看页面源码就能核对跑的是哪个版本
  other: { build: process.env.NEXT_PUBLIC_BUILD_SHA ?? "dev" },
};

export const viewport: Viewport = { width: "device-width", initialScale: 1 };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang={interfaceLocale}>
      <body className="min-h-dvh antialiased">
        <IntlProvider>{children}</IntlProvider>
      </body>
    </html>
  );
}
