import { type ReactNode } from "react";

import { cn } from "@/lib/cn";

interface CardProps {
  title?: string;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
  contentClassName?: string;
}

export function Card({ title, action, children, className, contentClassName }: CardProps) {
  return (
    <section
      className={cn(
        "overflow-hidden rounded-lg border border-border-darker bg-background",
        className,
      )}
    >
      {(title || action) && (
        <header className="flex items-center justify-between gap-3 border-b border-border px-5 py-3">
          {title && <h3 className="text-xs font-semibold text-foreground">{title}</h3>}
          {action}
        </header>
      )}
      <div className={cn("p-5", contentClassName)}>{children}</div>
    </section>
  );
}

export interface ChartCardProps {
  title: string;
  description?: string;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}

export function ChartCard({ title, description, action, children, className }: ChartCardProps) {
  return (
    <section
      className={cn(
        "overflow-hidden rounded-lg border border-border-darker bg-background",
        className,
      )}
    >
      <header className="flex items-start justify-between gap-3 border-b border-border px-5 py-3">
        <div className="min-w-0">
          <h3 className="text-xs font-semibold text-foreground">{title}</h3>
          {description && (
            <p className="mt-0.5 text-2xs text-muted-foreground">{description}</p>
          )}
        </div>
        {action && <div className="shrink-0">{action}</div>}
      </header>
      <div className="px-3 pb-3 pt-4">{children}</div>
    </section>
  );
}

interface MetricCardProps {
  label: string;
  value: string;
  sub?: string;
  accent?: "default" | "success" | "danger" | "info";
}

export function MetricCard({ label, value, sub, accent = "default" }: MetricCardProps) {
  const accentClass =
    accent === "success"
      ? "text-success"
      : accent === "danger"
        ? "text-danger"
        : accent === "info"
          ? "text-primary"
          : "text-foreground";

  return (
    <div className="rounded-lg border border-border-darker bg-background p-5">
      <p className="text-2xs font-medium uppercase tracking-wider text-muted-foreground">
        {label}
      </p>
      <p className={cn("mt-2 font-display text-2xl font-semibold tabular-nums tracking-tight", accentClass)}>
        {value}
      </p>
      {sub && <p className="mt-1 text-2xs text-muted-foreground">{sub}</p>}
    </div>
  );
}
