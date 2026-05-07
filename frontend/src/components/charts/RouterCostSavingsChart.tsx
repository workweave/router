"use client";

import { type TimeseriesBucket } from "@/lib/api";
import { useMemo } from "react";
import {
  Area,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import { AXIS_TICK, CHART_COLORS, TOOLTIP_STYLE } from "./chartTheme";

interface Props {
  buckets: TimeseriesBucket[];
  granularity: "hour" | "day";
  height?: number;
}

function formatUSD(v: number): string {
  if (v === 0) return "$0.00";
  if (Math.abs(v) < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

function formatBucket(iso: string, granularity: "hour" | "day"): string {
  const d = new Date(iso);
  if (granularity === "day") {
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
}

interface ChartPoint {
  bucket: string;
  label: string;
  requested: number;
  actual: number;
  savings_band: [number, number] | null;
  extra_band: [number, number] | null;
}

function buildPoint(bucket: string, label: string, actual: number, requested: number): ChartPoint {
  const diff = requested - actual;
  return {
    bucket,
    label,
    requested,
    actual,
    savings_band: diff >= 0 ? [actual, requested] : null,
    extra_band: diff < 0 ? [requested, actual] : null,
  };
}

export function RouterCostSavingsChart({ buckets, granularity, height = 240 }: Props) {
  const data = useMemo((): ChartPoint[] => {
    const points = buckets.map((b) => ({
      bucket: b.bucket,
      label: formatBucket(b.bucket, granularity),
      actual: b.actual_cost_usd,
      requested: b.requested_cost_usd,
    }));

    const result: ChartPoint[] = [];
    for (let i = 0; i < points.length; i++) {
      const cur = points[i];
      result.push(buildPoint(cur.bucket, cur.label, cur.actual, cur.requested));

      if (i < points.length - 1) {
        const next = points[i + 1];
        const d1 = cur.requested - cur.actual;
        const d2 = next.requested - next.actual;
        if (d1 * d2 < 0) {
          const ratio = d1 / (d1 - d2);
          const tMs =
            new Date(cur.bucket).getTime() +
            (new Date(next.bucket).getTime() - new Date(cur.bucket).getTime()) * ratio;
          const vStar = cur.actual + (next.actual - cur.actual) * ratio;
          const syntheticISO = new Date(tMs).toISOString();
          result.push(buildPoint(syntheticISO, "", vStar, vStar));
        }
      }
    }
    return result;
  }, [buckets, granularity]);

  return (
    <ResponsiveContainer width="100%" height={height}>
      <ComposedChart data={data} margin={{ top: 4, right: 16, left: 8, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_COLORS.border} vertical={false} />
        <XAxis
          dataKey="label"
          tick={AXIS_TICK}
          axisLine={false}
          tickLine={false}
          interval="equidistantPreserveStart"
        />
        <YAxis
          tickFormatter={(v) => formatUSD(v as number)}
          tick={AXIS_TICK}
          axisLine={false}
          tickLine={false}
          width={64}
        />
        <Tooltip
          formatter={(value, name) => {
            if (name === "Savings" || name === "Extra cost") return [null, null];
            return [formatUSD(value as number), name];
          }}
          labelFormatter={(label) => (label ? String(label) : null)}
          contentStyle={TOOLTIP_STYLE}
        />
        <Legend wrapperStyle={{ fontSize: 12 }} iconType="square" />

        <Area
          dataKey="savings_band"
          name="Savings"
          fill={CHART_COLORS.success}
          fillOpacity={0.15}
          stroke="none"
          legendType="square"
          dot={false}
          activeDot={false}
          isAnimationActive={false}
        />
        <Area
          dataKey="extra_band"
          name="Extra cost"
          fill={CHART_COLORS.danger}
          fillOpacity={0.15}
          stroke="none"
          legendType="square"
          dot={false}
          activeDot={false}
          isAnimationActive={false}
        />
        <Line
          dataKey="requested"
          name="Requested"
          stroke={CHART_COLORS.muted}
          strokeDasharray="4 4"
          strokeOpacity={0.7}
          dot={false}
          strokeWidth={1.5}
          isAnimationActive={false}
        />
        <Line
          dataKey="actual"
          name="Actual"
          stroke={CHART_COLORS.success}
          dot={false}
          strokeWidth={2}
          isAnimationActive={false}
        />
      </ComposedChart>
    </ResponsiveContainer>
  );
}
