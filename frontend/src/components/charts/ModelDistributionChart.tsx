"use client";

import { Text } from "@/components/atoms/Text";
import { type MetricsDetailRow } from "@/lib/api";
import { cn } from "@/lib/cn";
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from "recharts";
import { useMemo } from "react";

interface Props {
  rows: MetricsDetailRow[];
  isLoading?: boolean;
}

// Explicit class names so Tailwind's scanner includes them in the bundle.
const BAR_COLORS = [
  "bg-primary",
  "bg-primary/80",
  "bg-primary/60",
  "bg-primary/40",
  "bg-primary/20",
] as const;

const PROVIDER_COLORS: Record<string, string> = {
  anthropic:  "#CC785C",   // Anthropic brand terracotta
  openai:     "#000000",   // OpenAI black
  google:     "#4285F4",   // Google blue
  openrouter: "#6366F1",   // OpenRouter indigo
  fireworks:  "#FF6B35",   // Fireworks orange
  deepinfra:  "#0EA5E9",   // DeepInfra sky blue
  bedrock:    "#FF9900",   // AWS orange
};
const DEFAULT_PROVIDER_COLOR = "#94A3B8";

function providerColor(provider: string): string {
  return PROVIDER_COLORS[provider] ?? DEFAULT_PROVIDER_COLOR;
}

function capitalize(s: string): string {
  return s.length === 0 ? s : s[0]!.toUpperCase() + s.slice(1);
}

const RADIAN = Math.PI / 180;

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function renderCustomLabel({ cx, cy, midAngle, innerRadius, outerRadius, percent, name }: any) {
  if (percent < 0.15) return null;
  const radius = innerRadius + (outerRadius - innerRadius) * 0.5;
  const x = cx + radius * Math.cos(-midAngle * RADIAN);
  const y = cy + radius * Math.sin(-midAngle * RADIAN);
  return (
    <text
      x={x}
      y={y}
      fill="white"
      textAnchor="middle"
      dominantBaseline="central"
      fontSize={11}
      fontWeight={600}
    >
      {capitalize(name)}
    </text>
  );
}

export function ModelDistributionChart({ rows, isLoading = false }: Props) {
  const { modelStats, providerData } = useMemo(() => {
    const modelMap = new Map<string, { count: number; cost: number }>();
    const providerMap = new Map<string, number>();

    for (const row of rows) {
      if (row.decision_model !== "") {
        const prev = modelMap.get(row.decision_model) ?? { count: 0, cost: 0 };
        modelMap.set(row.decision_model, {
          count: prev.count + 1,
          cost: prev.cost + row.actual_cost_usd,
        });
      }

      if (row.decision_provider !== "") {
        providerMap.set(
          row.decision_provider,
          (providerMap.get(row.decision_provider) ?? 0) + 1,
        );
      }
    }

    const modelStats = Array.from(modelMap.entries())
      .map(([model, { count, cost }]) => ({ model, count, cost }))
      .sort((a, b) => b.count - a.count);

    const providerData = Array.from(providerMap.entries())
      .map(([name, value]) => ({ name, value }))
      .sort((a, b) => b.value - a.value);

    return { modelStats, providerData };
  }, [rows]);

  if (isLoading) {
    return (
      <div className="flex flex-col gap-3 p-2">
        {[0, 1, 2, 3].map(i => (
          <div key={i} className="flex items-center gap-3">
            <div className="h-3 w-32 animate-pulse rounded bg-muted" />
            <div
              className="h-3 animate-pulse rounded bg-muted"
              style={{ width: `${80 - i * 15}%` }}
            />
          </div>
        ))}
      </div>
    );
  }

  if (rows.length === 0 || (modelStats.length === 0 && providerData.length === 0)) {
    return (
      <div className="flex h-48 items-center justify-center text-sm text-muted-foreground">
        No data for this period
      </div>
    );
  }

  const maxModelCount = modelStats[0]?.count ?? 1;
  const totalRouted = modelStats.reduce((s, m) => s + m.count, 0);
  const totalForPie = providerData.reduce((s, e) => s + e.value, 0);

  return (
    <div className="flex flex-col gap-4 p-2">
      <div className="grid grid-cols-1 gap-6 @xl:grid-cols-2">
        {/* LEFT: model bars */}
        <div className="flex min-h-[280px] flex-col gap-3">
          <Text className="text-2xs font-medium uppercase tracking-wider text-muted-foreground">
            Models served
          </Text>
          <div className="flex flex-col gap-2">
            {modelStats.map(({ model, count }, i) => {
              const barPct = (count / maxModelCount) * 100;
              const sharePct = (totalRouted > 0 ? (count / totalRouted) * 100 : 0).toFixed(1);
              const colorClass = BAR_COLORS[Math.min(i, BAR_COLORS.length - 1)];
              return (
                <div key={model} className="flex w-full items-center gap-2">
                  <span
                    className="w-44 shrink-0 truncate font-mono text-2xs text-foreground"
                    title={model}
                  >
                    {model}
                  </span>
                  <div className="relative h-4 min-w-0 flex-1 overflow-hidden rounded-sm bg-muted/30">
                    <div
                      className={cn("h-full rounded-sm transition-all", colorClass)}
                      style={{ width: `${barPct}%` }}
                    />
                  </div>
                  <span className="w-24 shrink-0 text-right text-2xs tabular-nums text-muted-foreground">
                    {count} ({sharePct}%)
                  </span>
                </div>
              );
            })}
          </div>
        </div>

        {/* RIGHT: provider pie */}
        <div className="flex flex-col gap-3">
          <Text className="text-2xs font-medium uppercase tracking-wider text-muted-foreground">
            Provider split
          </Text>
          {providerData.length === 0 ? (
            <div className="flex h-48 items-center justify-center text-sm text-muted-foreground">
              No provider data
            </div>
          ) : (
            <>
              <ResponsiveContainer width="100%" height={220}>
                <PieChart>
                  <Pie
                    data={providerData}
                    dataKey="value"
                    cx="50%"
                    cy="50%"
                    outerRadius={90}
                    paddingAngle={3}
                    label={renderCustomLabel}
                    labelLine={false}
                  >
                    {providerData.map((entry, index) => (
                      <Cell
                        key={`cell-${index}`}
                        fill={providerColor(entry.name)}
                        stroke="white"
                        strokeWidth={2}
                      />
                    ))}
                  </Pie>
                  <Tooltip
                    formatter={(value) => {
                      const n = Number(value);
                      const pct = totalForPie > 0 ? ((n / totalForPie) * 100).toFixed(1) : "0.0";
                      return [`${n} requests (${pct}%)`, ""];
                    }}
                  />
                </PieChart>
              </ResponsiveContainer>

              <div className="mt-3 flex flex-wrap justify-center gap-x-4 gap-y-1.5">
                {providerData.map(entry => (
                  <div key={entry.name} className="flex items-center gap-1.5">
                    <div
                      className="h-2.5 w-2.5 shrink-0 rounded-full"
                      style={{ backgroundColor: providerColor(entry.name) }}
                    />
                    <span className="text-2xs capitalize text-muted-foreground">{entry.name}</span>
                    <span className="text-2xs font-medium tabular-nums text-foreground">
                      {((entry.value / totalForPie) * 100).toFixed(0)}%
                    </span>
                  </div>
                ))}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
