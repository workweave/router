"use client";

import { type TimeseriesBucket } from "@/lib/api";
import { useMemo } from "react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceLine,
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

function formatBucket(iso: string, granularity: "hour" | "day"): string {
  const d = new Date(iso);
  if (granularity === "day") {
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
}

export function SavingsRateChart({ buckets, granularity, height = 200 }: Props) {
  const data = useMemo(
    () =>
      buckets.map((b) => {
        const rate =
          b.requested_cost_usd > 0
            ? ((b.requested_cost_usd - b.actual_cost_usd) / b.requested_cost_usd) * 100
            : 0;
        return { label: formatBucket(b.bucket, granularity), rate };
      }),
    [buckets, granularity],
  );

  return (
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={data} margin={{ top: 4, right: 16, left: 8, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={CHART_COLORS.border} vertical={false} />
        <XAxis dataKey="label" tick={AXIS_TICK} axisLine={false} tickLine={false} />
        <YAxis
          tickFormatter={(v) => `${(v as number).toFixed(0)}%`}
          tick={AXIS_TICK}
          axisLine={false}
          tickLine={false}
          width={48}
          domain={["auto", "auto"]}
        />
        <ReferenceLine y={0} stroke={CHART_COLORS.borderDarker} strokeDasharray="2 2" />
        <Tooltip
          contentStyle={TOOLTIP_STYLE}
          formatter={(value) => [`${(value as number).toFixed(1)}%`, "Savings rate"]}
        />
        <Line
          type="monotone"
          dataKey="rate"
          stroke={CHART_COLORS.brand}
          strokeWidth={2}
          dot={false}
          isAnimationActive={false}
        />
      </LineChart>
    </ResponsiveContainer>
  );
}
