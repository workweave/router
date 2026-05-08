"use client";

import { Modal } from "@/components/molecules/Modal";
import { api, type MetricsDetailRow } from "@/lib/api";
import { cn } from "@/lib/cn";
import { useEffect, useState } from "react";

export interface DrillDownModalProps {
  /** ISO start of the bucket window (inclusive). */
  fromISO: string;
  /** ISO end of the bucket window (exclusive). */
  toISO: string;
  /** Human-readable title — used as the (sr-only) accessible name and on the footer. */
  title: string;
  /** Subtitle for screen readers / metric descriptor. */
  subtitle?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

function formatUSD(v: number): string {
  if (v === 0) return "$0.00";
  if (Math.abs(v) < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

function formatNumber(v: number): string {
  return v.toLocaleString();
}

function formatTimestamp(iso: string): string {
  const d = new Date(iso);
  const year = d.getFullYear();
  const month = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  let hours = d.getHours();
  const minutes = String(d.getMinutes()).padStart(2, "0");
  const ampm = hours >= 12 ? "pm" : "am";
  hours = hours % 12;
  if (hours === 0) hours = 12;
  return `${year}-${month}-${day} ${hours}:${minutes}${ampm}`;
}

/** Provider name → display label + Tailwind classes for the badge. Mirrors
 *  `internal/providers` constants on the Go side: anthropic, openai, google,
 *  openrouter. Unknown providers fall back to a neutral gray. */
const PROVIDER_BADGE: Record<
  string,
  { label: string; className: string }
> = {
  anthropic: {
    label: "Anthropic",
    className: "bg-brand/10 text-brand ring-brand/20",
  },
  openai: {
    label: "OpenAI",
    className: "bg-success/10 text-success ring-success/20",
  },
  google: {
    label: "Google",
    className: "bg-primary/10 text-primary ring-primary/20",
  },
  openrouter: {
    label: "OpenRouter",
    className: "bg-accent text-foreground ring-border",
  },
};

function ProviderBadge({ provider }: { provider: string }) {
  if (provider === "") return <>—</>;
  const meta = PROVIDER_BADGE[provider] ?? {
    label: provider,
    className: "bg-muted text-muted-foreground ring-border",
  };
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset",
        meta.className,
      )}
    >
      {meta.label}
    </span>
  );
}

interface ColumnDef {
  key: string;
  label: string;
  align?: "left" | "right";
  className?: string;
}

const COLUMNS: ColumnDef[] = [
  { key: "time", label: "Time", className: "w-[160px]" },
  { key: "requested", label: "Requested model" },
  { key: "decision", label: "Decision model" },
  { key: "provider", label: "Provider" },
  { key: "in_tok", label: "Input tokens", align: "right" },
  { key: "out_tok", label: "Output tokens", align: "right" },
  { key: "req_cost", label: "Requested $", align: "right" },
  { key: "act_cost", label: "Actual $", align: "right" },
  { key: "latency", label: "Latency", align: "right" },
  { key: "status", label: "Status", align: "right" },
];

export function DrillDownModal({
  fromISO,
  toISO,
  title,
  subtitle,
  open,
  onOpenChange,
}: DrillDownModalProps) {
  const [rows, setRows] = useState<MetricsDetailRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    setRows(null);
    api.metrics
      .details(fromISO, toISO, 200)
      .then(res => {
        if (cancelled) return;
        setRows(res.rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load details");
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, fromISO, toISO]);

  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <Modal.Content className="h-[80dvh] w-[90dvw] max-w-screen-2xl gap-0 p-0">
        <Modal.Title className="sr-only">{title}</Modal.Title>
        {subtitle != null && (
          <Modal.Description className="sr-only">{subtitle}</Modal.Description>
        )}

        <div className="h-full overflow-auto">
          {loading && (
            <div className="py-12 text-center text-sm text-muted-foreground">Loading…</div>
          )}
          {error != null && (
            <div className="m-6 rounded-md border border-danger/30 bg-danger/5 p-3 text-sm text-danger">
              {error}
            </div>
          )}
          {!loading && error == null && rows != null && rows.length === 0 && (
            <div className="py-12 text-center text-sm text-muted-foreground">
              No requests in this window.
            </div>
          )}
          {!loading && error == null && rows != null && rows.length > 0 && (
            <table className="w-full border-collapse text-sm">
              <thead className="sticky top-0 z-10 bg-background shadow-[inset_0_-1px_0_hsl(var(--border))]">
                <tr>
                  {COLUMNS.map(col => (
                    <th
                      key={col.key}
                      className={cn(
                        "px-4 py-3 text-sm font-medium text-foreground",
                        col.align === "right" ? "text-right" : "text-left",
                        col.className,
                      )}
                    >
                      {col.label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map((r, idx) => {
                  const isError = r.upstream_status_code >= 400;
                  const downgraded =
                    r.requested_model !== r.decision_model && r.decision_model !== "";
                  return (
                    <tr
                      key={`${r.request_id}-${idx}`}
                      className="border-t border-border/50 hover:bg-muted/30"
                    >
                      <td className="whitespace-nowrap px-4 py-3">
                        {formatTimestamp(r.timestamp)}
                      </td>
                      <td className="whitespace-nowrap px-4 py-3">{r.requested_model || "—"}</td>
                      <td
                        className={cn(
                          "whitespace-nowrap px-4 py-3",
                          downgraded && "text-success",
                        )}
                      >
                        {r.decision_model || "—"}
                      </td>
                      <td className="whitespace-nowrap px-4 py-3">
                        <ProviderBadge provider={r.decision_provider} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-3 text-right tabular-nums">
                        {formatNumber(r.input_tokens)}
                      </td>
                      <td className="whitespace-nowrap px-4 py-3 text-right tabular-nums">
                        {formatNumber(r.output_tokens)}
                      </td>
                      <td className="whitespace-nowrap px-4 py-3 text-right tabular-nums">
                        {formatUSD(r.requested_cost_usd)}
                      </td>
                      <td className="whitespace-nowrap px-4 py-3 text-right tabular-nums">
                        {formatUSD(r.actual_cost_usd)}
                      </td>
                      <td className="whitespace-nowrap px-4 py-3 text-right tabular-nums">
                        {r.total_latency_ms}ms
                      </td>
                      <td
                        className={cn(
                          "whitespace-nowrap px-4 py-3 text-right tabular-nums",
                          isError && "text-danger",
                        )}
                      >
                        {r.upstream_status_code || "—"}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>

      </Modal.Content>
    </Modal>
  );
}
