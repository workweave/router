"use client";

import { type TimeseriesBucket } from "@/lib/api";
import { useMemo } from "react";
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
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

export function CostBreakdownChart({ buckets, granularity, height = 220 }: Props) {
  const data = useMemo(
    () =>
      buckets.map((b) => {
        const savings = Math.max(0, b.requested_cost_usd - b.actual_cost_usd);
        return {
          label: formatBucket(b.bucket, granularity),
          actual: b.actual_cost_usd,
          savings,
        };
      }),
    [buckets, granularity],
  );

  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart data={data} margin={{ top: 4, right: 16, left: 8, bottom: 0 }} barCategoryGap={6}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_COLORS.border} vertical={false} />
        <XAxis dataKey="label" tick={AXIS_TICK} axisLine={false} tickLine={false} />
        <YAxis
          tickFormatter={(v) => formatUSD(v as number)}
          tick={AXIS_TICK}
          axisLine={false}
          tickLine={false}
          width={64}
        />
        <Tooltip
          contentStyle={TOOLTIP_STYLE}
          formatter={(value, name) => [formatUSD(value as number), name]}
        />
        <Legend wrapperStyle={{ fontSize: 12 }} iconType="square" />
        <Bar
          dataKey="actual"
          name="Actual cost"
          stackId="cost"
          fill={CHART_COLORS.primary}
          radius={[0, 0, 0, 0]}
          isAnimationActive={false}
        />
        <Bar
          dataKey="savings"
          name="Saved"
          stackId="cost"
          fill={CHART_COLORS.success}
          fillOpacity={0.55}
          radius={[3, 3, 0, 0]}
          isAnimationActive={false}
        />
      </BarChart>
    </ResponsiveContainer>
  );
}
