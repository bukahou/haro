"use client";

// 登录后的外壳。宽屏：全站只有一条侧栏 —— 上面新对话、日历、记账、我的，下面就是对话列表，左下角是自己的菜单（设置、管理员、登出）；
// 侧栏可以收成图标栏。窄屏（< 900px）：没有侧栏，底部四个标签，聊天标签里是对话列表。
import clsx from "clsx";
import {
  CalendarDays,
  KeyRound,
  LogOut,
  MessageCircle,
  MoreHorizontal,
  PanelLeft,
  Plus,
  Receipt,
  Search,
  Settings,
  Sparkles,
  User,
  Users,
  MonitorSmartphone,
  type LucideIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useState } from "react";

import { signOut } from "@/auth/logout";
import { ConversationList } from "@/components/chat/ConversationList";
import { OverlayHost, confirmDialog } from "@/components/overlay";
import { Avatar, Badge } from "@/components/ui";
import { pendingCount, useAccount } from "@/stores/account";
import { useClientConfig } from "@/stores/clientConfig";
import { useConversations } from "@/stores/conversations";
import { applyTheme, useUI } from "@/stores/ui";

const sections = [
  { href: "/calendar", key: "calendar", icon: CalendarDays },
  { href: "/expenses", key: "expenses", icon: Receipt },
  { href: "/me", key: "me", icon: User },
] as const;

const sidebarSearchId = "sidebar-search";

// 地址里的对话编号（/chat/<id>；new、search、backstory 不是对话）
function activeConversation(pathname: string): string | null {
  const match = pathname.match(/^\/chat\/([^/]+)$/);
  return match && !["new", "search", "backstory"].includes(match[1]) ? match[1] : null;
}

function MeMenu({ onClose, collapsed }: { onClose: () => void; collapsed: boolean }) {
  const t = useTranslations();
  const router = useRouter();
  const overview = useAccount((state) => state.overview);
  const admin = overview?.admin;
  const items: { href: string; label: string; icon: LucideIcon; badge?: number }[] = [{ href: "/me/settings", label: t("me.menu.settings"), icon: Settings }];
  const adminItems: { href: string; label: string; icon: LucideIcon; badge?: number }[] = admin
    ? [
        { href: "/me/admin/accounts", label: t("me.menu.accounts"), icon: Users, badge: admin.pending_accounts },
        { href: "/me/admin/terminals", label: t("me.menu.pendingTerminals"), icon: MonitorSmartphone, badge: admin.pending_terminals },
        { href: "/me/admin/invites", label: t("me.menu.invites"), icon: KeyRound },
      ]
    : [];
  const link = (item: (typeof items)[number]) => (
    <Link key={item.href} className="menu-item" href={item.href} onClick={onClose} role="menuitem">
      <item.icon size={18} aria-hidden />
      {item.label}
      <Badge count={item.badge} />
    </Link>
  );
  return (
    <>
      <div className="menu-layer" onClick={onClose} />
      <div className={clsx("menu", collapsed && "at-rail")} role="menu">
        {overview && (
          <div className="menu-head">
            <div className="b">{overview.account.display_name}</div>
            <div className="cap">{t(`me.roles.${overview.account.role}`)}</div>
          </div>
        )}
        {items.map(link)}
        {adminItems.length > 0 && (
          <>
            <div className="menu-sep" />
            <div className="menu-sec">{t("me.menu.admin")}</div>
            {adminItems.map(link)}
          </>
        )}
        <div className="menu-sep" />
        <button
          type="button"
          className="menu-item"
          role="menuitem"
          onClick={() => {
            onClose();
            confirmDialog({
              title: t("me.logoutConfirm.title"),
              body: t("me.logoutConfirm.body"),
              confirm: t("me.logout"),
              cancel: t("common.cancel"),
              run: async () => {
                await signOut();
                router.replace("/login");
              },
            });
          }}
        >
          <LogOut size={18} aria-hidden />
          {t("me.logout")}
        </button>
      </div>
    </>
  );
}

function RailItem({ icon: Icon, label, href, onClick, on, badge }: { icon: LucideIcon; label: string; href?: string; onClick?: () => void; on?: boolean; badge?: number }) {
  const inner = (
    <>
      <span className="pill">
        <Icon size={22} aria-hidden />
      </span>
      <span>{label}</span>
      <Badge count={badge} />
    </>
  );
  return href ? (
    <Link className={clsx("rail-item", on && "on")} href={href} title={label} aria-current={on ? "page" : undefined}>
      {inner}
    </Link>
  ) : (
    <button type="button" className="rail-item" onClick={onClick} title={label}>
      {inner}
    </button>
  );
}

function Sidebar() {
  const t = useTranslations();
  const pathname = usePathname();
  const router = useRouter();
  const collapsed = useUI((state) => state.sidebarCollapsed);
  const toggleSidebar = useUI((state) => state.toggleSidebar);
  const expandSidebar = useUI((state) => state.expandSidebar);
  const overview = useAccount((state) => state.overview);
  const [menuOpen, setMenuOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const pending = pendingCount(overview);
  const name = overview?.account.display_name ?? "";

  const isOn = (href: string) => pathname === href || pathname.startsWith(`${href}/`);
  const meOn = (href: string) => (href === "/me" ? isOn("/me") && !pathname.startsWith("/me/admin") && pathname !== "/me/settings" : isOn(href));

  if (collapsed) {
    return (
      <nav className="side rail" aria-label={t("shell.nav")}>
        <button type="button" className="icon-btn mut mb-2.5" onClick={toggleSidebar} aria-label={t("shell.expand")} title={t("shell.expand")}>
          <PanelLeft size={20} />
        </button>
        <RailItem icon={Plus} label={t("chat.new")} href="/chat/new" on={pathname === "/chat/new"} />
        <RailItem
          icon={Search}
          label={t("chat.search")}
          onClick={() => {
            expandSidebar();
            setTimeout(() => document.getElementById(sidebarSearchId)?.focus(), 0); // 展开后的侧栏画出来之后再放光标
          }}
        />
        {sections.map((section) => (
          <RailItem key={section.key} icon={section.icon} label={t(`tabs.${section.key}`)} href={section.href} on={meOn(section.href)} badge={section.key === "me" ? pending : 0} />
        ))}
        <button type="button" className="rail-me" onClick={() => setMenuOpen(true)} aria-label={t("shell.menu")}>
          <Avatar name={name} />
        </button>
        {menuOpen && <MeMenu collapsed onClose={() => setMenuOpen(false)} />}
      </nav>
    );
  }

  return (
    <nav className="side" aria-label={t("shell.nav")}>
      <div className="brand">
        <span className="logo">
          <Sparkles size={18} aria-hidden />
        </span>
        haro
        <button type="button" className="icon-btn mut ml-auto" onClick={toggleSidebar} aria-label={t("shell.collapse")} title={t("shell.collapse")}>
          <PanelLeft size={20} />
        </button>
      </div>
      <Link className={clsx("nav-item", pathname === "/chat/new" && "on")} href="/chat/new">
        <Plus size={20} aria-hidden />
        {t("chat.new")}
      </Link>
      {sections.map((section) => (
        <Link key={section.key} className={clsx("nav-item", meOn(section.href) && "on")} href={section.href} aria-current={meOn(section.href) ? "page" : undefined}>
          <section.icon size={20} aria-hidden />
          {t(`tabs.${section.key}`)}
          {section.key === "me" && <Badge count={pending} />}
        </Link>
      ))}
      <label className="search">
        <Search size={18} aria-hidden />
        <input
          id={sidebarSearchId}
          placeholder={t("chat.searchPlaceholder")}
          aria-label={t("chat.searchPlaceholder")}
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.nativeEvent.isComposing && filter.trim()) {
              router.push(`/chat/search?${new URLSearchParams({ q: filter.trim() }).toString()}`);
            }
          }}
        />
      </label>
      <div className="conv-items">
        <ConversationList activeId={activeConversation(pathname)} filter={filter} compact />
        {filter.trim() && (
          <Link className="sec-link px-2.5 py-2" href={`/chat/search?${new URLSearchParams({ q: filter.trim() }).toString()}`}>
            <Search size={14} aria-hidden />
            {t("chat.searchContent", { q: filter.trim() })}
          </Link>
        )}
      </div>
      <button type="button" className="me-chip" onClick={() => setMenuOpen(true)} aria-haspopup="menu">
        <Avatar name={name} />
        <span className="grow">
          <span className="b block">{name}</span>
          <span className="cap">{overview ? t(`me.roles.${overview.account.role}`) : ""}</span>
        </span>
        <Badge count={pending} />
        <MoreHorizontal size={18} aria-hidden />
      </button>
      {menuOpen && <MeMenu collapsed={false} onClose={() => setMenuOpen(false)} />}
    </nav>
  );
}

function TabBar() {
  const t = useTranslations("tabs");
  const pathname = usePathname();
  const pending = pendingCount(useAccount((state) => state.overview));
  const tabs = [{ href: "/chat", key: "chat", icon: MessageCircle }, ...sections] as const;
  return (
    <nav className="tabbar" aria-label={t("chat")}>
      {tabs.map(({ href, key, icon: Icon }) => {
        const on = pathname === href || pathname.startsWith(`${href}/`);
        return (
          <Link key={key} href={href} className={clsx("tab", on && "on")} aria-current={on ? "page" : undefined}>
            <Icon size={22} aria-hidden />
            {t(key)}
            {key === "me" && <Badge count={pending} />}
          </Link>
        );
      })}
    </nav>
  );
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const collapsed = useUI((state) => state.sidebarCollapsed);
  const theme = useUI((state) => state.theme);
  const refreshAccount = useAccount((state) => state.refresh);
  const loadConversations = useConversations((state) => state.load);
  useEffect(() => applyTheme(theme), [theme]);
  useEffect(() => {
    void refreshAccount();
    void loadConversations();
    void useClientConfig.getState().load();
    const onFocus = () => {
      void refreshAccount();
      void loadConversations();
    };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refreshAccount, loadConversations]);
  return (
    <>
      <Sidebar />
      <main className={clsx("main", collapsed && "collapsed")}>{children}</main>
      <TabBar />
      <OverlayHost />
    </>
  );
}
