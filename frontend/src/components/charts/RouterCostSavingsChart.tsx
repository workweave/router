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
const ACTUAL_COST_POS_KEY = "actual_cost_pos" as const;
const ACTUAL_COST_NEG_KEY = "actual_cost_neg" as const;
const REQUESTED_COST_KEY = "Requested cost" as const;
const SAVINGS_BAND_KEY = "savings_band" as const;
const EXTRA_COST_BAND_KEY = "extra_cost_band" as const;

type DependentKey =
  | typeof ACTUAL_COST_NEG_KEY
  | typeof ACTUAL_COST_POS_KEY
  | typeof EXTRA_COST_BAND_KEY
  | typeof REQUESTED_COST_KEY
  | typeof SAVINGS_BAND_KEY;
type DependentValue = [number, number] | number;

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

const SERIES_CONFIG: Partial<Record<DependentKey, ChartSeriesConfig<DateTime, DependentValue>>> = {
  [ACTUAL_COST_NEG_KEY]: {
    color: ChartColor.Red,
    formatValue: v => formatUSD(typeof v === "number" ? v : v[0]),
    label: "Actual cost",
    legend: false,
    showTooltip: false,
    type: ChartSeriesType.Line,
  },
  [ACTUAL_COST_POS_KEY]: {
    color: ChartColor.Green,
    formatValue: v => formatUSD(typeof v === "number" ? v : v[0]),
    label: "Actual cost",
    type: ChartSeriesType.Line,
  },
  [EXTRA_COST_BAND_KEY]: {
    color: ChartColor.Red,
    interactive: false,
    label: "Extra cost",
    legend: true,
    opacity: 0.15,
    showTooltip: false,
    type: ChartSeriesType.Area,
  },
  [REQUESTED_COST_KEY]: {
    color: ChartColor.Scale2,
    dashed: true,
    formatValue: v => formatUSD(typeof v === "number" ? v : v[1]),
    label: "Requested cost",
    opacity: 0.6,
    type: ChartSeriesType.Line,
  },
  [SAVINGS_BAND_KEY]: {
    color: ChartColor.Green,
    interactive: false,
    label: "Savings",
    legend: true,
    opacity: 0.15,
    showTooltip: false,
    type: ChartSeriesType.Area,
  },
};

const DEPENDENT_KEYS = LoadState.loaded([
  SAVINGS_BAND_KEY,
  EXTRA_COST_BAND_KEY,
  REQUESTED_COST_KEY,
  ACTUAL_COST_POS_KEY,
  ACTUAL_COST_NEG_KEY,
] as readonly DependentKey[]);

function buildChartPoint(time: DateTime, actual: number, requested: number) {
  const diff = requested - actual;
  return {
    [ACTUAL_COST_NEG_KEY]: diff <= 0 ? (actual as DependentValue) : null,
    [ACTUAL_COST_POS_KEY]: actual as DependentValue,
    [EXTRA_COST_BAND_KEY]: diff <= 0 ? ([requested, actual] as DependentValue) : null,
    [REQUESTED_COST_KEY]: requested as DependentValue,
    [SAVINGS_BAND_KEY]: diff >= 0 ? ([actual, requested] as DependentValue) : null,
    [TIME_KEY]: time,
  };
}

/** Map the router's "hour"/"day"/"week" to the WW TimeGranularity enum
 *  used by the chart helpers. */
function toTimeGranularity(g: RouterGranularity): TimeGranularity {
  switch (g) {
    case "hour":
      // No "Hour" entry exists in WW's TimeGranularity; the chart helpers
      // (formatTime/formatInterval/addGranularity) operate on Day or larger.
      // Treat "hour" as Day for axis labelling purposes; the actual time
      // resolution comes from the data itself.
      return TimeGranularity.Day;
    case "day":
      return TimeGranularity.Day;
    case "week":
      return TimeGranularity.Week;
  }
}

/**
 * Difference/band chart for router cost savings, ported from
 * frontend/components/charts/RouterCostSavingsChart.tsx. Renders two lines
 * (requested vs actual cost) with the gap shaded green for savings and red
 * for overspend; sign changes get a synthetic interpolation point so band
 * polygons meet seamlessly.
 */
export function RouterCostSavingsChart({ buckets, granularity }: Props) {
  const wwGranularity = toTimeGranularity(granularity);
  const drilldown = useChartDrillDown(granularity);

  const { chartData, syntheticTimes } = useMemo(() => {
    const synthetic = new Set<DateTime>();
    const sortedBuckets = [...buckets].sort(
      (a, b) => new Date(a.bucket).getTime() - new Date(b.bucket).getTime(),
    );
    const points = sortedBuckets.map(b => ({
      time: new Date(b.bucket).getTime() as DateTime,
      actual: b.actual_cost_usd,
      requested: b.requested_cost_usd,
    }));

    const result: ReturnType<typeof buildChartPoint>[] = [];
    for (let i = 0; i < points.length; i++) {
      const cur = points[i];
      result.push(buildChartPoint(cur.time, cur.actual, cur.requested));

      if (i < points.length - 1) {
        const next = points[i + 1];
        const d1 = cur.requested - cur.actual;
        const d2 = next.requested - next.actual;
        if (d1 * d2 < 0) {
          const ratio = d1 / (d1 - d2);
          const tStar = (cur.time + (next.time - cur.time) * ratio) as DateTime;
          const vStar = cur.actual + (next.actual - cur.actual) * ratio;
          synthetic.add(tStar);
          result.push(buildChartPoint(tStar, vStar, vStar));
        }
      }
    }
    return { chartData: LoadState.loaded(result), syntheticTimes: synthetic };
  }, [buckets]);

  const actualCostLegendColor = useMemo(() => {
    const data = LoadState.unwrap(chartData);
    if (data == null) return ChartColor.Green;
    const hasNonNegativeSegment = data.some(p => p[SAVINGS_BAND_KEY] != null);
    return hasNonNegativeSegment ? ChartColor.Green : ChartColor.Red;
  }, [chartData]);

  const config = useMemo(
    () =>
      actualCostLegendColor === ChartColor.Green ?
        SERIES_CONFIG
      : {
          ...SERIES_CONFIG,
          [ACTUAL_COST_POS_KEY]: {
            ...SERIES_CONFIG[ACTUAL_COST_POS_KEY],
            color: actualCostLegendColor,
          },
        },
    [actualCostLegendColor],
  );

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
        config={config}
        formatValue={v => formatUSDCompact(typeof v === "number" ? v : v[1])}
        formatIndependentValue={value =>
          syntheticTimes.has(value) ? "" : formatTime(value, wwGranularity)
        }
        formatIndependentValueTooltip={value =>
          syntheticTimes.has(value) ? null : formatInterval(value, wwGranularity)
        }
        legend
        onClickDataPoint={time => {
          if (syntheticTimes.has(time)) return;
          drilldown.open(time);
        }}
      />
      {drilldown.state != null && (
        <DrillDownModal
          fromISO={drilldown.state.fromISO}
          toISO={drilldown.state.toISO}
          title={drilldown.state.title}
          subtitle="Router cost savings — requests in this bucket"
          open={drilldown.isOpen}
          onOpenChange={isOpen => {
            if (!isOpen) drilldown.close();
          }}
        />
      )}
    </>
  );
}
