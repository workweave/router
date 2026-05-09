import { cn } from "@/tools/cn";

import { useLayoutEffect, useRef } from "react";
import * as Recharts from "recharts";

import type { ChartProps } from "./Chart";
import { useChartContext } from "./ChartContext";
import { TOOLTIP_DATA_KEY } from "./constants";
import { ChartDataKeyType, ChartDataPoint, ChartDataValueType } from "./types";

export interface ChartTooltipProps<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
  TTooltipData,
> extends Partial<Recharts.TooltipContentProps<TDependentValue, TDependentKey>>,
    Pick<React.ComponentProps<"div">, "className">,
    Pick<
      ChartProps<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>,
      "formatValue"
    > {
  /**
   * Formatter for the independent values in the chart tooltip.
   * Can return a React node to display rich content like avatars.
   */
  formatIndependentValue?: (value: TIndependentValue) => React.ReactNode;
  /**
   * Formatter for the independent values that takes the active series into account.
   * If provided, this is used instead of formatIndependentValue.
   * Useful for showing different dates based on which series is being hovered.
   */
  formatIndependentValueForSeries?: (
    value: TIndependentValue,
    activeSeriesKey: ChartDataKeyType | null,
  ) => React.ReactNode;
  /**
   * If true, the label will not be shown.
   *
   * @default false
   */
  hideLabel?: boolean;
  /**
   * If true, the tooltip is locked in place and will not be hidden when moving the mouse away from
   * the given data point.
   *
   * @default false
   */
  locked?: boolean;
  /**
   * If provided, used to render additional data in the tooltip.
   * Receives the tooltip data and the independent value (label) for the hovered data point.
   */
  renderTooltipData?: (data: TTooltipData, independentValue: TIndependentValue | null) => React.ReactNode;

  /**
   * If true, inactive series will be faded out.
   *
   * @default true
   */
  fadeInactiveSeries?: boolean;
}

/**
 * Renders the content for the chart tooltip.
 */
export function ChartTooltip<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
  TTooltipData,
>({
  active,
  className,
  fadeInactiveSeries = true,
  formatIndependentValue,
  formatIndependentValueForSeries,
  formatValue,
  hideLabel = false,
  label,
  locked = false,
  payload,
  renderTooltipData,
}: ChartTooltipProps<
  TIndependentKey,
  TDependentKey,
  TIndependentValue,
  TDependentValue,
  TTooltipData
>) {
  const { activeSeries } = useChartContext();
  const tooltipRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const el = tooltipRef.current;
    if (el == null) return;

    el.style.transform = "";

    const rect = el.getBoundingClientRect();
    const chartWrapper = el.closest(".recharts-wrapper");
    if (chartWrapper == null) return;
    const chartRect = chartWrapper.getBoundingClientRect();

    let offsetX = 0;
    let offsetY = 0;

    if (rect.right > chartRect.right) {
      offsetX = chartRect.right - rect.right - 8;
    }
    if (rect.left + offsetX < chartRect.left) {
      offsetX = chartRect.left - rect.left + 8;
    }

    if (rect.bottom > chartRect.bottom) {
      offsetY = chartRect.bottom - rect.bottom - 8;
    }
    if (rect.top + offsetY < chartRect.top) {
      offsetY = chartRect.top - rect.top + 8;
    }

    const transforms: string[] = [];
    if (offsetX !== 0) transforms.push(`translateX(${offsetX}px)`);
    if (offsetY !== 0) transforms.push(`translateY(${offsetY}px)`);

    if (transforms.length > 0) {
      el.style.transform = transforms.join(" ");
    }
    // Only re-measure when tooltip content or lock state changes. Without this, every parent
    // re-render (e.g. on every mousemove) would force a synchronous layout reflow via
    // getBoundingClientRect, causing jank on complex pages.
  }, [active, payload, label, locked]);

  if (!locked && (active === false || payload == null || payload.length === 0)) return null;
  if (payload == null || payload.length === 0) return null;
  const independentValue = label as TIndependentValue | null | undefined;

  // Use series-aware formatter if provided, otherwise fall back to basic formatter
  const title =
    independentValue != null && formatIndependentValueForSeries != null ?
      formatIndependentValueForSeries(independentValue, activeSeries ?? null)
    : independentValue != null && formatIndependentValue != null ?
      formatIndependentValue(independentValue)
    : independentValue != null ? String(independentValue)
    : undefined;

  let tooltipData: TTooltipData | undefined;

  type PayloadItem = RechartsTooltipPayloadItem<TDependentKey, TDependentValue>;
  const typedPayload = payload as ReadonlyArray<PayloadItem>;

  if (renderTooltipData != null) {
    tooltipData = typedPayload
      .map(
        p =>
          p.payload as ChartDataPoint<
            TIndependentKey,
            TDependentKey,
            TIndependentValue,
            TDependentValue,
            TTooltipData
          >,
      )
      .find(p => p[TOOLTIP_DATA_KEY] != null)?.[TOOLTIP_DATA_KEY];
  }

  const sortedPayload = typedPayload.slice().sort((a, b) => {
    if (activeSeries == null) return 0;

    const aIsActive = a.name === activeSeries;
    const bIsActive = b.name === activeSeries;

    if (aIsActive && !bIsActive) return -1;
    if (!aIsActive && bIsActive) return 1;
    return 0;
  });

  return (
    <div
      ref={tooltipRef}
      className={cn(
        "grid max-h-96 min-w-[8rem] items-start gap-3 overflow-auto rounded-lg border bg-background p-2 text-xs shadow-xl",
        { "border-border-darker": locked },
        className,
      )}
      onClick={locked ? e => e.stopPropagation() : undefined}
    >
      {!hideLabel && title != null && <div className="font-medium">{title}</div>}

      <div className="flex w-full flex-col gap-1.5">
        {sortedPayload.map(series => (
          <ChartTooltipSeries
            key={`${String(series.dataKey)}-${series.type}`}
            formatValue={formatValue}
            independentValue={independentValue}
            series={series}
            fadeInactiveSeries={fadeInactiveSeries}
          />
        ))}
      </div>

      {tooltipData != null && renderTooltipData?.(tooltipData, independentValue ?? null)}
    </div>
  );
}

/** A single item in the payload for a Recharts tooltip. */
type RechartsTooltipPayloadItem<
  TDependentKey extends ChartDataKeyType,
  TDependentValue extends ChartDataValueType,
> = Omit<Recharts.TooltipPayloadEntry<TDependentValue, TDependentKey>, "name" | "value"> & {
  name?: TDependentKey;
  value?: TDependentValue;
};

interface ChartTooltipSeriesProps<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
> extends Pick<
      ChartProps<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>,
      "formatValue"
    >,
    Pick<
      ChartTooltipProps<
        TIndependentKey,
        TDependentKey,
        TIndependentValue,
        TDependentValue,
        unknown
      >,
      "fadeInactiveSeries"
    > {
  independentValue: TIndependentValue | null | undefined;
  series: Pick<
    RechartsTooltipPayloadItem<TDependentKey, TDependentValue>,
    "color" | "name" | "type" | "value"
  >;
}

/**
 * Renders the content for a single series in the chart tooltip.
 */
function ChartTooltipSeries<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
>({
  fadeInactiveSeries = true,
  formatValue: globalFormatValue = String,
  independentValue,
  series,
}: ChartTooltipSeriesProps<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>) {
  const { activeSeries, config } = useChartContext();

  if (series.name == null) return null;
  if (series.type === "none") return null;

  const seriesConfig = config?.[series.name];

  const tooltip = seriesConfig?.showTooltip;
  if (tooltip === false) return null;
  if (typeof tooltip === "function" && independentValue != null && !tooltip(independentValue)) {
    return null;
  }

  const icon = seriesConfig?.icon;
  const color = seriesConfig?.color ?? series.color;
  const formatValue = seriesConfig?.formatValue ?? globalFormatValue;
  const otherSeriesActive = activeSeries != null && activeSeries !== series.name;

  return (
    <div
      className={cn("flex w-full flex-wrap items-center gap-2", {
        "opacity-30": fadeInactiveSeries && otherSeriesActive,
      })}
    >
      {icon != null ?
        <span className="text-muted-foreground">{icon}</span>
      : color != null ?
        <div
          className="size-2.5 shrink-0 rounded-sm bg-[--color-bg]"
          style={{ "--color-bg": color, "--color-border": color } as React.CSSProperties}
        />
      : null}

      <div className="flex flex-1 items-center gap-4">
        <span className="text-muted-foreground">{seriesConfig?.labelNode ?? seriesConfig?.label ?? series.name}</span>

        <span
          className={cn(
            "ml-auto font-mono font-medium tabular-nums",
            series.value != null ? "text-foreground" : "text-muted-foreground",
          )}
        >
          {series.value != null ? formatValue(series.value) : "-"}
        </span>
      </div>
    </div>
  );
}
