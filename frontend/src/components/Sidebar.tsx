"use client";

import { BarChart2, LogOut, Settings } from "lucide-react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { type ReactNode } from "react";

import { Logo } from "./Logo";
import { api } from "@/lib/api";
import { cn } from "@/lib/cn";

interface NavItem {
  href: string;
  label: string;
  icon: ReactNode;
  matchPrefix?: string;
}

const NAV_ITEMS: NavItem[] = [
  { href: "/dashboard", label: "Dashboard", icon: <BarChart2 size={16} /> },
];

function NavButton({ item }: { item: NavItem }) {
  const pathname = usePathname();
  const active = item.matchPrefix
    ? pathname.startsWith(item.matchPrefix)
    : pathname === item.href || pathname.startsWith(item.href + "/");

  return (
    <Link
      href={item.href}
      aria-selected={active}
      title={item.label}
      className={cn(
        "relative flex h-7 w-full items-center gap-2 rounded-md px-3 text-xs font-medium text-foreground transition-colors",
        "hover:bg-foreground/5 active:bg-foreground/10",
        "aria-selected:bg-foreground/5",
      )}
    >
      <span className="shrink-0">{item.icon}</span>
      <span className="hidden whitespace-nowrap md:inline">{item.label}</span>
    </Link>
  );
}

export function Sidebar() {
  const pathname = usePathname();
  const router = useRouter();
  const settingsActive = pathname.startsWith("/settings");

  async function handleSignOut() {
    try {
      await api.auth.logout();
    } catch {
      // Best-effort: even if the network call fails, redirect to /login so
      // the user is no longer in a half-authed state.
    }
    router.replace("/login");
  }

  return (
    <div className="group/sidebar relative flex h-full w-12 shrink-0 grow-0 flex-col items-start gap-1 overflow-hidden transition-all duration-200 ease-out md:w-[244px] md:overflow-visible">
      <header className="relative z-10 flex w-full flex-col items-center gap-4 py-2 transition-all duration-200 md:flex-row md:pl-2 md:pr-3 md:pt-2">
        <Logo href="/dashboard" />
      </header>

      <div className="relative z-10 flex w-full flex-1 flex-col gap-4 overflow-y-auto md:p-2 md:pt-0">
        <nav className="flex w-full flex-col gap-4">
          <div className="flex w-full flex-col gap-1">
            {NAV_ITEMS.map((item) => (
              <NavButton key={item.href} item={item} />
            ))}
          </div>
        </nav>
      </div>

      <div className="fixed bottom-4 left-4 right-4 z-10 flex items-center gap-2 md:right-auto">
        <Link
          href="/settings"
          aria-selected={settingsActive}
          title="Settings"
          className={cn(
            "inline-flex size-8 shrink-0 items-center justify-center rounded-md border border-border-darker bg-background p-0 text-muted-foreground transition-colors",
            "hover:bg-foreground/5 hover:text-foreground",
            "aria-selected:bg-foreground/5 aria-selected:text-foreground",
          )}
        >
          <Settings size={16} />
        </Link>
        <button
          type="button"
          onClick={handleSignOut}
          title="Sign out"
          className={cn(
            "inline-flex size-8 shrink-0 items-center justify-center rounded-md border border-border-darker bg-background p-0 text-muted-foreground transition-colors",
            "hover:bg-foreground/5 hover:text-foreground",
          )}
        >
          <LogOut size={16} />
        </button>
      </div>
    </div>
  );
}
