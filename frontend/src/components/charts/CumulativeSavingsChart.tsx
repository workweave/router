"use client";

import { type TimeseriesBucket } from "@/lib/api";
import { useMemo } from "react";
import {
  Area,
  AreaChart,
  CartesianGrid,
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

export function CumulativeSavingsChart({ buckets, granularity, height = 220 }: Props) {
  const data = useMemo(() => {
    let cum = 0;
    return buckets.map((b) => {
      cum += b.requested_cost_usd - b.actual_cost_usd;
      return {
        label: formatBucket(b.bucket, granularity),
        cumulative: cum,
      };
    });
  }, [buckets, granularity]);

  return (
    <ResponsiveContainer width="100%" height={height}>
      <AreaChart data={data} margin={{ top: 4, right: 16, left: 8, bottom: 0 }}>
        <defs>
          <linearGradient id="gradCumSavings" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={CHART_COLORS.success} stopOpacity={0.35} />
            <stop offset="100%" stopColor={CHART_COLORS.success} stopOpacity={0.02} />
          </linearGradient>
        </defs>
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
          formatter={(value) => [formatUSD(value as number), "Cumulative savings"]}
        />
        <Area
          type="monotone"
          dataKey="cumulative"
          stroke={CHART_COLORS.success}
          strokeWidth={2}
          fill="url(#gradCumSavings)"
          isAnimationActive={false}
        />
      </AreaChart>
    </ResponsiveContainer>
  );
}
