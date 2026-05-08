"use client";

import { Text } from "@/components/atoms/Text";
import { CostBreakdownChart } from "@/components/charts/CostBreakdownChart";
import { CumulativeSavingsChart } from "@/components/charts/CumulativeSavingsChart";
import { RouterCostSavingsChart } from "@/components/charts/RouterCostSavingsChart";
import { SavingsRateChart } from "@/components/charts/SavingsRateChart";
import {
  DashboardPageFilters,
  useDashboardFilters,
} from "@/components/DashboardPageFilters";
import { Card } from "@/components/molecules/Card";
import { Page } from "@/components/Page";
import { PageHeader } from "@/components/PageHeader";
import { ResponsiveGrid } from "@/components/ResponsiveGrid";
import { api, type MetricsSummary, type TimeseriesBucket } from "@/lib/api";
import { cn } from "@/lib/cn";
import { useEffect, useState } from "react";

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
  const dashboardFilters = useDashboardFilters("30d");
  const { fromISO, toISO, granularity, range } = dashboardFilters.filters;

  const [summary, setSummary] = useState<MetricsSummary | null>(null);
  const [buckets, setBuckets] = useState<TimeseriesBucket[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    Promise.all([
      api.metrics.summary(fromISO, toISO),
      api.metrics.timeseries(granularity, fromISO, toISO),
    ])
      .then(([s, ts]) => {
        if (cancelled) return;
        setSummary(s);
        setBuckets(ts.buckets ?? []);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load metrics");
      });
    return () => {
      cancelled = true;
    };
  }, [fromISO, toISO, granularity]);

  if (error) {
    return (
      <Page
        header={
          <PageHeader
            left={
              <Text
                variant="h4"
                as="h2"
                className="flex flex-row items-center gap-1 whitespace-nowrap"
              >
                Dashboard
              </Text>
            }
          />
        }
      >
        <Page.Section>
          <div className="rounded-lg border border-danger/30 bg-danger/5 p-4 text-sm text-danger">
            {error}
          </div>
        </Page.Section>
      </Page>
    );
  }

  const savingsRate =
    summary == null || summary.total_requested_cost_usd === 0
      ? 0
      : (summary.total_savings_usd / summary.total_requested_cost_usd) * 100;
  const avgTokensPerReq =
    summary == null || summary.request_count === 0
      ? 0
      : summary.total_tokens / summary.request_count;
  const empty = buckets.length === 0;

  return (
    <Page
      header={
        <PageHeader
          left={
            <Text
              variant="h4"
              as="h2"
              className="flex flex-row items-center gap-1 whitespace-nowrap"
            >
              Router cost &amp; savings
            </Text>
          }
        />
      }
      subheader={<DashboardPageFilters result={dashboardFilters} />}
    >
      <Page.Section className="pt-0">
        <ResponsiveGrid>
          <MetricCard
            className={ResponsiveGrid.Small}
            label="Cost saved"
            value={summary == null ? "—" : formatUSD(Math.abs(summary.total_savings_usd))}
            sub={
              summary == null
                ? undefined
                : summary.total_savings_usd >= 0
                  ? `${savingsRate.toFixed(1)}% of requested`
                  : "Over requested cost"
            }
            accent={
              summary == null
                ? "default"
                : summary.total_savings_usd >= 0
                  ? "success"
                  : "danger"
            }
          />
          <MetricCard
            className={ResponsiveGrid.Small}
            label="Requests"
            value={summary == null ? "—" : formatNumber(summary.request_count)}
            sub={
              summary == null ? undefined : `actual ${formatUSD(summary.total_actual_cost_usd)}`
            }
          />
          <MetricCard
            className={ResponsiveGrid.Small}
            label="Tokens"
            value={summary == null ? "—" : formatNumber(summary.total_tokens)}
            sub={summary == null ? undefined : `${formatNumber(avgTokensPerReq)} avg / req`}
          />

          <ChartCard
            className={ResponsiveGrid.Full}
            title="Router cost savings"
            description="Actual cost vs. what would have been charged for the requested model."
            action={
              summary != null && summary.total_savings_usd !== 0 ? (
                <span
                  className={cn(
                    "text-2xs font-medium",
                    summary.total_savings_usd >= 0 ? "text-success" : "text-danger",
                  )}
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
              <RouterCostSavingsChart buckets={buckets} granularity={granularity} />
            )}
          </ChartCard>

          <ChartCard
            className={ResponsiveGrid.Medium}
            title="Cumulative savings"
            description={`Running total of dollars saved across the ${range.label.toLowerCase()}.`}
          >
            {empty ? (
              <EmptyChart height={220} />
            ) : (
              <CumulativeSavingsChart buckets={buckets} granularity={granularity} />
            )}
          </ChartCard>

          <ChartCard
            className={ResponsiveGrid.Medium}
            title="Savings rate"
            description="Percent of requested cost avoided per bucket."
          >
            {empty ? (
              <EmptyChart height={200} />
            ) : (
              <SavingsRateChart buckets={buckets} granularity={granularity} />
            )}
          </ChartCard>

          <ChartCard
            className={ResponsiveGrid.Full}
            title="Cost breakdown per bucket"
            description="Actual cost stacked with realized savings."
          >
            {empty ? (
              <EmptyChart height={220} />
            ) : (
              <CostBreakdownChart buckets={buckets} granularity={granularity} />
            )}
          </ChartCard>
        </ResponsiveGrid>
      </Page.Section>
    </Page>
  );
}

interface MetricCardProps {
  className?: string;
  label: string;
  value: string;
  sub?: string;
  accent?: "default" | "success" | "danger" | "info";
}

function MetricCard({ className, label, value, sub, accent = "default" }: MetricCardProps) {
  const accentClass =
    accent === "success"
      ? "text-success"
      : accent === "danger"
        ? "text-danger"
        : accent === "info"
          ? "text-primary"
          : "text-foreground";

  return (
    <Card size="sm" className={className}>
      <Card.Header>
        <Text className="text-2xs font-medium uppercase tracking-wider text-muted-foreground">
          {label}
        </Text>
      </Card.Header>
      <Card.Content>
        <Text
          className={cn(
            "font-display text-2xl font-semibold tabular-nums tracking-tight",
            accentClass,
          )}
        >
          {value}
        </Text>
        {sub != null && (
          <Text className="mt-1 text-2xs text-muted-foreground">{sub}</Text>
        )}
      </Card.Content>
    </Card>
  );
}

interface ChartCardProps {
  className?: string;
  title: string;
  description?: string;
  action?: React.ReactNode;
  children: React.ReactNode;
}

function ChartCard({ className, title, description, action, children }: ChartCardProps) {
  return (
    <Card size="sm" className={className}>
      <Card.Header className="flex-row items-start justify-between gap-3">
        <div className="min-w-0">
          <Card.Title variant="h4">{title}</Card.Title>
          {description != null && (
            <Card.Description className="mt-0.5 text-2xs">{description}</Card.Description>
          )}
        </div>
        {action != null && <div className="shrink-0">{action}</div>}
      </Card.Header>
      <Card.Content>{children}</Card.Content>
    </Card>
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
