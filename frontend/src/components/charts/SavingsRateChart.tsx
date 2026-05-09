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
const RATE_KEY = "rate" as const;

type DependentKey = typeof RATE_KEY;

type RouterGranularity = "hour" | "day" | "week";

interface Props {
  buckets: TimeseriesBucket[];
  granularity: RouterGranularity;
}

const SERIES_CONFIG: Partial<Record<DependentKey, ChartSeriesConfig<DateTime, number>>> = {
  [RATE_KEY]: {
    color: ChartColor.Orange1,
    formatValue: v => `${(typeof v === "number" ? v : v[0]).toFixed(1)}%`,
    label: "Savings rate",
    type: ChartSeriesType.Line,
  },
};

const DEPENDENT_KEYS = LoadState.loaded([RATE_KEY] as readonly DependentKey[]);

function toTimeGranularity(g: RouterGranularity): TimeGranularity {
  return g === "week" ? TimeGranularity.Week : TimeGranularity.Day;
}

export function SavingsRateChart({ buckets, granularity }: Props) {
  const wwGranularity = toTimeGranularity(granularity);
  const drilldown = useChartDrillDown(granularity);

  const chartData = useMemo(() => {
    const sorted = [...buckets].sort(
      (a, b) => new Date(a.bucket).getTime() - new Date(b.bucket).getTime(),
    );
    return LoadState.loaded(
      sorted.map(b => {
        const rate =
          b.requested_cost_usd > 0
            ? ((b.requested_cost_usd - b.actual_cost_usd) / b.requested_cost_usd) * 100
            : 0;
        return {
          [TIME_KEY]: new Date(b.bucket).getTime() as DateTime,
          [RATE_KEY]: rate,
        };
      }),
    );
  }, [buckets]);

  const referenceLines = useMemo(
    () =>
      LoadState.loaded([
        { type: "dependent" as const, value: 0, dashed: true, label: undefined, side: undefined },
      ]),
    [],
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
        config={SERIES_CONFIG}
        formatValue={v => `${(typeof v === "number" ? v : v[0]).toFixed(0)}%`}
        formatIndependentValue={value => formatTime(value, wwGranularity)}
        formatIndependentValueTooltip={value => formatInterval(value, wwGranularity)}
        referenceLines={referenceLines}
        onClickDataPoint={time => drilldown.open(time)}
      />
      {drilldown.state != null && (
        <DrillDownModal
          fromISO={drilldown.state.fromISO}
          toISO={drilldown.state.toISO}
          title={drilldown.state.title}
          subtitle="Savings rate — requests in this bucket"
          open={drilldown.isOpen}
          onOpenChange={isOpen => {
            if (!isOpen) drilldown.close();
          }}
        />
      )}
    </>
  );
}
