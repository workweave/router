"use client";

import {
  Chart,
  ChartAxisType,
  ChartColor,
  ChartSeriesConfig,
  ChartSeriesType,
} from "@/components/Chart";
import { type TimeseriesBucket } from "@/lib/api";
import { DateTime } from "@/objects/scalars/DateTime";
import { TimeGranularity, formatInterval, formatTime } from "@/objects/TimeGranularity";
import { LoadState } from "@/tools/LoadState";
import { useMemo } from "react";

import { DrillDownModal } from "./DrillDownModal";
import { useChartDrillDown } from "./useChartDrillDown";

const TIME_KEY = "time" as const;
const ACTUAL_KEY = "actual" as const;
const SAVINGS_KEY = "savings" as const;

type DependentKey = typeof ACTUAL_KEY | typeof SAVINGS_KEY;

type RouterGranularity = "hour" | "day" | "week";

interface Props {
  buckets: TimeseriesBucket[];
  granularity: RouterGranularity;
}

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

const SERIES_CONFIG: Partial<Record<DependentKey, ChartSeriesConfig<DateTime, number>>> = {
  [ACTUAL_KEY]: {
    color: ChartColor.Scale2,
    formatValue: v => formatUSD(typeof v === "number" ? v : v[0]),
    label: "Actual cost",
    type: ChartSeriesType.Bar,
  },
  [SAVINGS_KEY]: {
    color: ChartColor.Green,
    formatValue: v => formatUSD(typeof v === "number" ? v : v[0]),
    label: "Saved",
    opacity: 0.55,
    type: ChartSeriesType.Bar,
  },
};

const DEPENDENT_KEYS = LoadState.loaded([ACTUAL_KEY, SAVINGS_KEY] as readonly DependentKey[]);

// Stack actual + savings into one bar per bucket.
const GROUPS = LoadState.loaded([[ACTUAL_KEY, SAVINGS_KEY]] as readonly (readonly DependentKey[])[]);

function toTimeGranularity(g: RouterGranularity): TimeGranularity {
  return g === "week" ? TimeGranularity.Week : TimeGranularity.Day;
}

export function CostBreakdownChart({ buckets, granularity }: Props) {
  const wwGranularity = toTimeGranularity(granularity);
  const drilldown = useChartDrillDown(granularity);

  const chartData = useMemo(() => {
    const sorted = [...buckets].sort(
      (a, b) => new Date(a.bucket).getTime() - new Date(b.bucket).getTime(),
    );
    return LoadState.loaded(
      sorted.map(b => ({
        [TIME_KEY]: new Date(b.bucket).getTime() as DateTime,
        [ACTUAL_KEY]: b.actual_cost_usd,
        [SAVINGS_KEY]: Math.max(0, b.requested_cost_usd - b.actual_cost_usd),
      })),
    );
  }, [buckets]);

  return (
    <>
      <Chart
        data={chartData}
        dependentKeys={DEPENDENT_KEYS}
        groups={GROUPS}
        independentKey={TIME_KEY}
        independentType={ChartAxisType.Category}
        config={SERIES_CONFIG}
        formatValue={v => formatUSDCompact(typeof v === "number" ? v : v[1])}
        formatIndependentValue={value => formatTime(value, wwGranularity)}
        formatIndependentValueTooltip={value => formatInterval(value, wwGranularity)}
        legend
        onClickDataPoint={time => drilldown.open(time)}
      />
      {drilldown.state != null && (
        <DrillDownModal
          fromISO={drilldown.state.fromISO}
          toISO={drilldown.state.toISO}
          title={drilldown.state.title}
          subtitle="Cost breakdown — requests in this bucket"
          open={drilldown.isOpen}
          onOpenChange={isOpen => {
            if (!isOpen) drilldown.close();
          }}
        />
      )}
    </>
  );
}
