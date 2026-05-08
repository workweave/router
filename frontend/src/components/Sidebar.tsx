"use client";

import { Logo } from "./Logo";
import { Button } from "@/components/molecules/Button";
import { Popover } from "@/components/molecules/Popover";
import { Tooltip } from "@/components/molecules/Tooltip";
import { Appearance, Intent } from "@/components/types";
import { api } from "@/lib/api";
import { cn } from "@/lib/cn";
import { BarChart2, LogOut, Settings } from "lucide-react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { type ReactNode } from "react";

interface NavItem {
  href: string;
  label: string;
  icon: ReactNode;
  matchPrefix?: string;
}

const PRIMARY_NAV: NavItem[] = [
  { href: "/dashboard", label: "Dashboard", icon: <BarChart2 size={16} /> },
];

function NavLink({ item }: { item: NavItem }) {
  const pathname = usePathname();
  const active =
    item.matchPrefix != null
      ? pathname.startsWith(item.matchPrefix)
      : pathname === item.href || pathname.startsWith(item.href + "/");

  return (
    <Link
      href={item.href}
      aria-selected={active}
      title={item.label}
      className={cn(
        "relative flex h-8 w-full items-center gap-2 rounded-md px-3 text-xs font-medium text-muted-foreground transition-colors",
        "hover:bg-foreground/5 hover:text-foreground",
        "aria-selected:bg-foreground/5 aria-selected:text-foreground",
      )}
    >
      <span className="shrink-0">{item.icon}</span>
      <span className="hidden whitespace-nowrap md:inline">{item.label}</span>
    </Link>
  );
}

export function Sidebar() {
  const router = useRouter();

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

      <nav className="relative z-10 flex w-full flex-1 flex-col gap-1 overflow-y-auto md:p-2 md:pt-0">
        {PRIMARY_NAV.map(item => (
          <NavLink key={item.href} item={item} />
        ))}
      </nav>

      <div className="relative z-10 flex w-full items-center px-2 py-2 md:p-2">
        <Popover>
          <Tooltip content="Menu" side="right" interactiveChild>
            <Popover.Trigger>
              <Button
                appearance={Appearance.Hollow}
                className="size-8 shrink-0 justify-center rounded-md border border-border-darker bg-muted p-0 text-muted-foreground hover:bg-border-darker hover:text-foreground"
                title="Menu"
              >
                <Settings className="size-4" />
              </Button>
            </Popover.Trigger>
          </Tooltip>

          <Popover.Content side="top" align="start" sideOffset={8} className="w-64 p-1">
            <div className="flex flex-col">
              <Button
                href="/settings"
                appearance={Appearance.Hollow}
                className="h-7 w-full justify-start gap-2 px-2 text-xs"
              >
                <Settings className="size-4 shrink-0 text-muted-foreground" />
                <span>Settings</span>
              </Button>

              <Button
                appearance={Appearance.Hollow}
                className="h-7 w-full justify-start gap-2 px-2 text-xs"
                onClick={() => {
                  void handleSignOut();
                }}
              >
                <LogOut className="size-4 shrink-0 text-muted-foreground" />
                <span>Sign out</span>
              </Button>
            </div>
          </Popover.Content>
        </Popover>
      </div>
    </div>
  );
}
