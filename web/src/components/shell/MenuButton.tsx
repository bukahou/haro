"use client";

// 窄屏的页头左边：把侧栏从左边滑出来（宽屏时由 haro.css 藏掉）。有管理员待处理的事时带个红点。
import { Menu } from "lucide-react";
import { useTranslations } from "next-intl";

import { pendingCount, useAccount } from "@/stores/account";
import { useUI } from "@/stores/ui";

export function MenuButton() {
  const t = useTranslations("shell");
  const setDrawer = useUI((state) => state.setDrawer);
  const pending = pendingCount(useAccount((state) => state.overview));
  return (
    <button type="button" className="icon-btn mut menu-btn" onClick={() => setDrawer(true)} aria-label={t("openMenu")} title={t("openMenu")}>
      <Menu size={22} />
      {pending > 0 && <span className="badge" aria-hidden />}
    </button>
  );
}
