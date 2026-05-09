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
import { TimeGranularity, addGranularity, formatInterval, formatTime } from "@/objects/TimeGranularity";
import { LoadState } from "@/tools/LoadState";
import { useMemo } from "react";

import { DrillDownModal } from "./DrillDownModal";
import { useChartDrillDown } from "./useChartDrillDown";

const TIME_KEY = "time" as const;
const CUMULATIVE_KEY = "cumulative" as const;

type DependentKey = typeof CUMULATIVE_KEY;

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
  [CUMULATIVE_KEY]: {
    color: ChartColor.Green,
    formatValue: v => formatUSD(typeof v === "number" ? v : v[0]),
    label: "Cumulative savings",
    type: ChartSeriesType.Area,
  },
};

const DEPENDENT_KEYS = LoadState.loaded([CUMULATIVE_KEY] as readonly DependentKey[]);

function toTimeGranularity(g: RouterGranularity): TimeGranularity {
  return g === "week" ? TimeGranularity.Week : TimeGranularity.Day;
}

export function CumulativeSavingsChart({ buckets, granularity }: Props) {
  const wwGranularity = toTimeGranularity(granularity);
  const drilldown = useChartDrillDown(granularity);

  const chartData = useMemo(() => {
    const sorted = [...buckets].sort(
      (a, b) => new Date(a.bucket).getTime() - new Date(b.bucket).getTime(),
    );
    let cum = 0;
    return LoadState.loaded(
      sorted.map(b => {
        cum += b.requested_cost_usd - b.actual_cost_usd;
        return {
          [TIME_KEY]: new Date(b.bucket).getTime() as DateTime,
          [CUMULATIVE_KEY]: cum,
        };
      }),
    );
  }, [buckets]);

  return (
    <>
      <Chart
        data={chartData}
        dependentKeys={DEPENDENT_KEYS}
        independentKey={TIME_KEY}
        independentType={ChartAxisType.Time}
        independentDomain={([min, max]) => [
          DateTime.fromDate(addGranularity(min, wwGranularity, -1)),
          DateTime.fromDate(addGranularity(max, wwGranularity, 1)),
        ]}
        config={SERIES_CONFIG}
        formatValue={v => formatUSDCompact(typeof v === "number" ? v : v[1])}
        formatIndependentValue={value => formatTime(value, wwGranularity)}
        formatIndependentValueTooltip={value => formatInterval(value, wwGranularity)}
        onClickDataPoint={time => drilldown.open(time)}
      />
      {drilldown.state != null && (
        <DrillDownModal
          fromISO={drilldown.state.fromISO}
          toISO={drilldown.state.toISO}
          title={drilldown.state.title}
          subtitle="Cumulative savings — requests in this bucket"
          open={drilldown.isOpen}
          onOpenChange={isOpen => {
            if (!isOpen) drilldown.close();
          }}
        />
      )}
    </>
  );
}
