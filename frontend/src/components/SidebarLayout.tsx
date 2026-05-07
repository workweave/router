import { type ReactNode } from "react";

interface SidebarLayoutProps {
  sidebar: ReactNode;
  children: ReactNode;
}

export function SidebarLayout({ sidebar, children }: SidebarLayoutProps) {
  return (
    <div className="flex h-full w-full flex-row items-start justify-center gap-2 overflow-hidden bg-muted p-2">
      {sidebar}
      <div className="h-full w-full flex-grow overflow-hidden rounded-lg border-[0.5px] border-border-darker bg-background">
        <div className="h-full w-full overflow-y-auto">{children}</div>
      </div>
    </div>
  );
}
