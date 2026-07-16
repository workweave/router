"use client";

import {
  Chart,
  ChartAxisType,
  ChartColor,
  ChartSeriesConfig,
  ChartSeriesType,
} from "@/components/Chart";
import { type ModelBreakdownBucket } from "@/lib/api";
import { DateTime } from "@/objects/scalars/DateTime";
import { TimeGranularity, formatInterval, formatTime } from "@/objects/TimeGranularity";
import { LoadState } from "@/tools/LoadState";
import { useMemo } from "react";

const TIME_KEY = "time" as const;

type RouterGranularity = "hour" | "day" | "week";

export type ModelBreakdownMetric = "requests" | "spend";

interface Props {
  buckets: ModelBreakdownBucket[];
  granularity: RouterGranularity;
  metric: ModelBreakdownMetric;
}

const MODEL_COLORS: readonly ChartColor[] = [
  ChartColor.Scale1,
  ChartColor.Scale2,
  ChartColor.Green,
  ChartColor.Orange2,
  ChartColor.Scale3,
  ChartColor.GoalLine,
  ChartColor.Benchmark1,
  ChartColor.BenchmarkP90,
  ChartColor.Benchmark3,
  ChartColor.Red,
];

function formatUSD(v: number): string {
  if (v === 0) return "$0.00";
  if (Math.abs(v) < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

function formatUSDCompact(v: number): string {
  const abs = Math.abs(v);
  if (abs >= 1000) return `$${(v / 1000).toFixed(1)}K`;
  if (abs >= 1) return `$${v.toFixed(0)}`;
  return `$${v.toFixed(2)}`;
}

function formatCount(v: number): string {
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`;
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K`;
  return String(Math.round(v));
}

function toTimeGranularity(g: RouterGranularity): TimeGranularity {
  return g === "week" ? TimeGranularity.Week : TimeGranularity.Day;
}

/**
 * Stacked bar chart of per-bucket totals broken down by the model the router
 * selected. `metric` picks the summed value: request volume (usage) or actual
 * retail cost (spend) — mirroring the managed WorkWeave dashboard's Model
 * Usage / Model Spend charts.
 */
export function ModelBreakdownChart({ buckets, granularity, metric }: Props) {
  const wwGranularity = toTimeGranularity(granularity);

  const formatValue = metric === "spend" ? formatUSD : formatCount;
  const formatAxisValue = metric === "spend" ? formatUSDCompact : formatCount;

  const { chartData, models, config } = useMemo(() => {
    const modelNames = [...new Set(buckets.map(b => b.decision_model))].sort();

    const byTime = new Map<number, Record<string, number>>();
    for (const b of buckets) {
      const t = new Date(b.bucket).getTime();
      const row = byTime.get(t) ?? {};
      row[b.decision_model] =
        (row[b.decision_model] ?? 0) +
        (metric === "spend" ? b.actual_cost_usd : b.request_count);
      byTime.set(t, row);
    }

    const data = [...byTime.entries()]
      .sort(([a], [b]) => a - b)
      .map(([t, row]) => ({
        [TIME_KEY]: t as DateTime,
        ...Object.fromEntries(modelNames.map(m => [m, row[m] ?? 0])),
      }));

    const seriesConfig: Partial<Record<string, ChartSeriesConfig<DateTime, number>>> =
      Object.fromEntries(
        modelNames.map((m, i) => [
          m,
          {
            color: MODEL_COLORS[i % MODEL_COLORS.length],
            formatValue: (v: number | readonly number[]) =>
              formatValue(typeof v === "number" ? v : v[0]),
            label: m === "" ? "(unknown)" : m,
            type: ChartSeriesType.Bar,
          },
        ]),
      );

    return {
      chartData: LoadState.loaded(data),
      models: modelNames,
      config: seriesConfig,
    };
  }, [buckets, metric, formatValue]);

  return (
    <Chart
      data={chartData}
      dependentKeys={LoadState.loaded(models)}
      groups={LoadState.loaded([models])}
      independentKey={TIME_KEY}
      independentType={ChartAxisType.Category}
      config={config}
      formatValue={v => formatAxisValue(typeof v === "number" ? v : v[1])}
      formatIndependentValue={value => formatTime(value, wwGranularity)}
      formatIndependentValueTooltip={value => formatInterval(value, wwGranularity)}
      legend
    />
  );
}
