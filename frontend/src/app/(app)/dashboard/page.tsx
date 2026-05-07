"use client";

import { ChartCard, MetricCard } from "@/components/Card";
import { CostBreakdownChart } from "@/components/charts/CostBreakdownChart";
import { CumulativeSavingsChart } from "@/components/charts/CumulativeSavingsChart";
import { RouterCostSavingsChart } from "@/components/charts/RouterCostSavingsChart";
import { SavingsRateChart } from "@/components/charts/SavingsRateChart";
import { PageBody, PageHeader } from "@/components/PageHeader";
import { api, type MetricsSummary, type TimeseriesBucket } from "@/lib/api";
import { useEffect, useState } from "react";

type Granularity = "hour" | "day";

const RANGE_OPTIONS: Array<{ label: string; days: number; granularity: Granularity }> = [
  { label: "24h", days: 1, granularity: "hour" },
  { label: "7d", days: 7, granularity: "day" },
  { label: "30d", days: 30, granularity: "day" },
];

function formatUSD(v: number): string {
  if (v === 0) return "$0.00";
  if (Math.abs(v) < 0.001) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

function formatNumber(v: number): string {
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`;
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K`;
  return String(v);
}

export default function DashboardPage() {
  const [summary, setSummary] = useState<MetricsSummary | null>(null);
  const [buckets, setBuckets] = useState<TimeseriesBucket[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [rangeIdx, setRangeIdx] = useState(2); // default 30d
  const range = RANGE_OPTIONS[rangeIdx];

  useEffect(() => {
    const to = new Date();
    const from = new Date(to);
    from.setDate(from.getDate() - range.days);

    const fromISO = from.toISOString();
    const toISO = to.toISOString();

    Promise.all([
      api.metrics.summary(fromISO, toISO),
      api.metrics.timeseries(range.granularity, fromISO, toISO),
    ])
      .then(([s, ts]) => {
        setSummary(s);
        setBuckets(ts.buckets ?? []);
        setError(null);
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Failed to load metrics");
      });
  }, [range.days, range.granularity]);

  if (error) {
    return (
      <PageBody>
        <div className="rounded-lg border border-danger/30 bg-danger/5 p-4 text-sm text-danger">
          {error.includes("401") ? (
            <span>
              Authentication required.{" "}
              <button
                className="underline"
                onClick={() => {
                  const token = prompt("Enter your router API key (rk_...)");
                  if (token) {
                    localStorage.setItem("router_token", token);
                    window.location.reload();
                  }
                }}
              >
                Set API key
              </button>
            </span>
          ) : (
            error
          )}
        </div>
      </PageBody>
    );
  }

  const savingsAccent =
    summary == null
      ? "default"
      : summary.total_savings_usd >= 0
        ? "success"
        : "danger";

  const savingsRate =
    summary == null || summary.total_requested_cost_usd === 0
      ? 0
      : (summary.total_savings_usd / summary.total_requested_cost_usd) * 100;

  const avgTokensPerReq =
    summary == null || summary.request_count === 0
      ? 0
      : summary.total_tokens / summary.request_count;

  const RangeToggle = (
    <div className="flex items-center gap-1 rounded-md border border-border-darker bg-background p-0.5">
      {RANGE_OPTIONS.map((opt, idx) => (
        <button
          key={opt.label}
          type="button"
          onClick={() => setRangeIdx(idx)}
          aria-selected={rangeIdx === idx}
          className="rounded px-2.5 py-1 text-2xs font-medium text-muted-foreground transition-colors hover:text-foreground aria-selected:bg-muted aria-selected:text-foreground"
        >
          {opt.label}
        </button>
      ))}
    </div>
  );

  const empty = buckets.length === 0;

  return (
    <>
      <PageHeader
        title="Dashboard"
        description="Routing performance, cost savings, and request volume across your installation."
        actions={RangeToggle}
      />
      <PageBody>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <MetricCard
            label="Cost saved"
            value={summary == null ? "—" : formatUSD(Math.abs(summary.total_savings_usd))}
            sub={
              summary == null
                ? undefined
                : summary.total_savings_usd >= 0
                  ? `${savingsRate.toFixed(1)}% of requested`
                  : "Over requested cost"
            }
            accent={savingsAccent}
          />
          <MetricCard
            label="Requests"
            value={summary == null ? "—" : formatNumber(summary.request_count)}
            sub={
              summary == null
                ? undefined
                : `actual cost ${formatUSD(summary.total_actual_cost_usd)}`
            }
          />
          <MetricCard
            label="Tokens"
            value={summary == null ? "—" : formatNumber(summary.total_tokens)}
            sub={summary == null ? undefined : `${formatNumber(avgTokensPerReq)} avg / req`}
          />
          <MetricCard
            label="Requested cost"
            value={summary == null ? "—" : formatUSD(summary.total_requested_cost_usd)}
            sub={summary == null ? undefined : "If routed to requested model"}
            accent="info"
          />
        </div>

        <ChartCard
          title="Router cost savings"
          description="Actual cost vs. what would have been charged for the requested model."
          action={
            summary != null && summary.total_savings_usd !== 0 ? (
              <span
                className={`text-2xs font-medium ${summary.total_savings_usd >= 0 ? "text-success" : "text-danger"}`}
              >
                {summary.total_savings_usd >= 0
                  ? `${formatUSD(summary.total_savings_usd)} saved`
                  : `${formatUSD(Math.abs(summary.total_savings_usd))} extra`}
              </span>
            ) : null
          }
        >
          {empty ? (
            <EmptyChart />
          ) : (
            <RouterCostSavingsChart buckets={buckets} granularity={range.granularity} />
          )}
        </ChartCard>

        <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
          <ChartCard
            title="Cumulative savings"
            description="Running total of dollars saved over the period."
          >
            {empty ? <EmptyChart height={220} /> : (
              <CumulativeSavingsChart buckets={buckets} granularity={range.granularity} />
            )}
          </ChartCard>

          <ChartCard
            title="Savings rate"
            description="Percent of requested cost avoided per bucket."
          >
            {empty ? <EmptyChart height={200} /> : (
              <SavingsRateChart buckets={buckets} granularity={range.granularity} />
            )}
          </ChartCard>
        </div>

        <ChartCard
          title="Cost breakdown per bucket"
          description="Actual cost stacked with realized savings."
        >
          {empty ? <EmptyChart height={220} /> : (
            <CostBreakdownChart buckets={buckets} granularity={range.granularity} />
          )}
        </ChartCard>
      </PageBody>
    </>
  );
}

function EmptyChart({ height = 240 }: { height?: number }) {
  return (
    <div
      className="flex items-center justify-center text-2xs text-muted-foreground"
      style={{ height }}
    >
      No data for this period
    </div>
  );
}
