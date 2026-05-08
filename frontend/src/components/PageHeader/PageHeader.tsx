import { cn } from "@/lib/cn";
import React from "react";

export interface PageHeaderProps {
  left?: React.ReactNode;
  leftColumnClassName?: string;
  right?: React.ReactNode;
}

/**
 * Page header with a left content slot (title / breadcrumbs) and a
 * right action slot. Matches the WW dashboard header chrome but
 * without the global SearchBar — the router doesn't have search.
 */
export function PageHeader({ left, leftColumnClassName, right }: PageHeaderProps) {
  return (
    <div className="flex w-full min-w-0 items-center gap-6">
      <div className={cn("min-w-0 flex-[0_1_auto] overflow-hidden pr-1", leftColumnClassName)}>
        {left}
      </div>
      <div className="min-w-0 flex-1 basis-0" />
      {right != null && <div className="shrink-0">{right}</div>}
    </div>
  );
}
