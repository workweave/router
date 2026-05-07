"use client";

import { useEffect, useState } from "react";
import { usePathname, useRouter } from "next/navigation";

import { Sidebar } from "@/components/Sidebar";
import { SidebarLayout } from "@/components/SidebarLayout";
import { api } from "@/lib/api";

type AuthState = "checking" | "authed" | "anonymous";

export default function AppLayout({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const [state, setState] = useState<AuthState>("checking");

  useEffect(() => {
    let cancelled = false;
    api.auth
      .me()
      .then((res) => {
        if (cancelled) return;
        if (res.authenticated) {
          setState("authed");
        } else {
          setState("anonymous");
          const next = encodeURIComponent(pathname || "/dashboard");
          router.replace(`/login?next=${next}`);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setState("anonymous");
          router.replace("/login");
        }
      });
    return () => {
      cancelled = true;
    };
  }, [pathname, router]);

  if (state !== "authed") {
    return <SidebarLayout sidebar={<div className="md:w-[244px]" />}>{null}</SidebarLayout>;
  }

  return <SidebarLayout sidebar={<Sidebar />}>{children}</SidebarLayout>;
}
