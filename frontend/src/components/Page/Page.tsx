import { cn } from "@/lib/cn";
import React from "react";

export interface PageProps extends React.PropsWithChildren {
  className?: string;
  header: React.ReactNode;
  subheader?: React.ReactNode;
}

/**
 * Renders a page (excluding the sidebar) with a header strip and an
 * optional sub-header row. Body is scrollable and constrained to a max
 * content width so dashboards don't sprawl on ultra-wide displays.
 */
export function Page({ children, className, header, subheader }: PageProps) {
  return (
    <div className="grid h-full w-full grid-cols-[minmax(0,_1fr)] grid-rows-[auto_minmax(0,1fr)]">
      <div>
        <header className="flex min-h-12 w-full min-w-0 flex-row items-center border-b border-b-border px-6 py-2 pr-2">
          {header}
        </header>
        {subheader != null && (
          <div className="flex w-full flex-row items-end border-b border-b-border px-3">
            {subheader}
          </div>
        )}
      </div>
      <div
        className={cn(
          "relative flex h-full w-full flex-col items-center overflow-auto @container [scrollbar-gutter:stable]",
          className,
        )}
      >
        {children}
      </div>
    </div>
  );
}

export interface PageSectionProps extends React.HTMLAttributes<HTMLDivElement> {
  header?: React.ReactNode;
}

Page.Section = React.forwardRef<HTMLDivElement, PageSectionProps>(
  ({ children, className, header, ...props }, ref) => (
    <section
      className={cn("flex w-full max-w-content-width flex-col gap-4 p-6", className)}
      ref={ref}
      {...props}
    >
      {header}
      {children}
    </section>
  ),
);
(Page.Section as React.ForwardRefExoticComponent<unknown>).displayName = "Page.Section";

Page.SectionHeader = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  function PageSectionHeader({ children, className, ...props }, ref) {
    return (
      <div
        className={cn("group relative flex flex-row items-center gap-2", className)}
        ref={ref}
        {...props}
      >
        {children}
      </div>
    );
  },
);
(Page.SectionHeader as React.ForwardRefExoticComponent<unknown>).displayName = "Page.SectionHeader";
