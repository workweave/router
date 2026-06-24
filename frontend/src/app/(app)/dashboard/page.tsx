"use client";

import { Text } from "@/components/atoms/Text";
import { ChartCard } from "@/components/ChartCard";
import { CostBreakdownChart } from "@/components/charts/CostBreakdownChart";
import { CumulativeSavingsChart } from "@/components/charts/CumulativeSavingsChart";
import { ModelDistributionChart } from "@/components/charts/ModelDistributionChart";
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
import { Statistic } from "@/components/Statistic";
import { api, type MetricsDetailRow, type MetricsSummary, type TimeseriesBucket } from "@/lib/api";
import { cn } from "@/lib/cn";
import { LoadState } from "@/tools/LoadState";
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
  const [detailRows, setDetailRows] = useState<MetricsDetailRow[]>([]);
  const [detailRowsLoading, setDetailRowsLoading] = useState(true);
  const [chartsLoading, setChartsLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    setSummary(null);
    setBuckets([]);
    setChartsLoading(true);
    Promise.all([
      api.metrics.summary(fromISO, toISO),
      api.metrics.timeseries(granularity, fromISO, toISO),
    ])
      .then(([s, ts]) => {
        if (cancelled) return;
        setSummary(s);
        setBuckets(ts.buckets ?? []);
        setChartsLoading(false);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setChartsLoading(false);
        setError(err instanceof Error ? err.message : "Failed to load metrics.");
      });
    return () => {
      cancelled = true;
    };
  }, [fromISO, toISO, granularity]);

  useEffect(() => {
    let cancelled = false;
    setDetailRows([]);
    setDetailRowsLoading(true);
    api.metrics.details(fromISO, toISO, 1000)
      .then(d => {
        if (cancelled) return;
        setDetailRows(d.rows ?? []);
        setDetailRowsLoading(false);
      })
      .catch(() => {
        if (cancelled) return;
        setDetailRowsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [fromISO, toISO]);

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
  const empty = !chartsLoading && buckets.length === 0;

  const routedRows = detailRows.filter(r => r.decision_model !== "");
  const substitutedRows = routedRows.filter(
    r => r.decision_model !== r.requested_model,
  );

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
    >
      <Page.Section>
        <DashboardPageFilters result={dashboardFilters} />
        <ResponsiveGrid>
          <div className={cn(ResponsiveGrid.Full, "grid grid-cols-2 gap-4 @xl:grid-cols-4")}>
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
              accent={
                summary == null
                  ? "default"
                  : summary.total_savings_usd >= 0
                    ? "success"
                    : "danger"
              }
            />
            <MetricCard
              label="Requests"
              value={summary == null ? "—" : formatNumber(summary.request_count)}
              sub={
                summary == null ? undefined : `actual ${formatUSD(summary.total_actual_cost_usd)}`
              }
            />
            <MetricCard
              label="Tokens"
              value={summary == null ? "—" : formatNumber(summary.total_tokens)}
              sub={summary == null ? undefined : `${formatNumber(avgTokensPerReq)} avg / req`}
            />
            <MetricCard
              label="Substitution rate"
              value={
                routedRows.length === 0
                  ? "—"
                  : detailRows.length === 1000
                    ? `~${Math.round((substitutedRows.length / routedRows.length) * 100)}%`
                    : `${Math.round((substitutedRows.length / routedRows.length) * 100)}%`
              }
              sub={
                summary == null || routedRows.length === 0
                  ? undefined
                  : detailRows.length === 1000
                    ? `${new Set(routedRows.map(r => r.decision_model)).size} models (sampled)`
                    : `${new Set(routedRows.map(r => r.decision_model)).size} models served`
              }
              accent={substitutedRows.length > 0 ? "info" : "default"}
            />
          </div>

          <ChartCard
            className={ResponsiveGrid.Full}
            title="Router cost savings"
            subtitle="Actual cost vs. what would have been charged for the requested model."
            topRight={
              <Statistic
                statistic={
                  summary == null
                    ? LoadState.loading()
                    : LoadState.loaded({
                        total:
                          summary.total_savings_usd >= 0
                            ? `${formatUSD(summary.total_savings_usd)} saved`
                            : `${formatUSD(Math.abs(summary.total_savings_usd))} extra`,
                      })
                }
              />
            }
          >
            {empty ? (
              <EmptyChart />
            ) : (
              <RouterCostSavingsChart buckets={buckets} granularity={granularity} />
            )}
          </ChartCard>

          <div className={cn(ResponsiveGrid.Full, "rounded-xl border bg-card shadow-sm")}>
            <div className="border-b px-4 py-3 md:px-6">
              <p className="text-sm font-semibold leading-tight text-foreground">
                Model distribution
              </p>
              <p className="mt-0.5 text-xs text-muted-foreground">
                Models and providers for requests served in the selected period.
              </p>
              {detailRows.length === 1000 && (
                <p className="mt-0.5 text-xs text-muted-foreground/60">
                  Showing first 1,000 requests — distribution may be a sample.
                </p>
              )}
            </div>
            <div className="p-4 md:p-6">
              <ModelDistributionChart rows={detailRows} isLoading={detailRowsLoading} />
            </div>
          </div>

          <ChartCard
            className={ResponsiveGrid.Medium}
            title="Cumulative savings"
            subtitle={`Running total of dollars saved across the ${range.label.toLowerCase()}.`}
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
            subtitle="Percent of requested cost avoided per bucket."
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
            subtitle="Actual cost stacked with realized savings."
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
