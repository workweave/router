import { Tooltip } from "@/components/molecules/Tooltip";
import { cn } from "@/tools/cn";

import * as Recharts from "recharts";

import { useChartContext } from "./ChartContext";
import { ChartDataKeyType } from "./types";

const LEGEND_TEXT_MAX_LENGTH = 40;

function truncateText(text: string, maxLength: number): string {
  if (text.length <= maxLength) return text;
  return text.slice(0, maxLength - 3) + "...";
}

export interface ChartLegendProps extends React.HTMLAttributes<HTMLDivElement> {
  payload?: ReadonlyArray<Recharts.LegendPayload>;
}

/**
 * Renders a legend for the chart, linking colors to the series they are associated with.
 */
export function ChartLegend({ className, payload }: ChartLegendProps) {
  return (
    <div
      className={cn(
        "flex max-h-24 flex-wrap items-center justify-center gap-x-1 overflow-y-auto",
        className,
      )}
    >
      {payload?.map((series, idx) => (
          <ChartLegendSeries key={`${series.value}-${series.type}-${idx}`} series={series} />
        ))}
    </div>
  );
}

/** A single item in the payload Recharts passes to the legend. */
type RechartsLegendPayloadItem = Recharts.LegendPayload;

interface ChartLegendSeriesProps {
  series: Pick<RechartsLegendPayloadItem, "color" | "type" | "value">;
}

function ChartLegendSeries<TDependentKey extends ChartDataKeyType>({
  series,
}: ChartLegendSeriesProps) {
  const { activeSeries, config, hiddenSeries, setActiveSeries, toggleSeriesVisibility } =
    useChartContext();

  const seriesName = series.value as TDependentKey | undefined;
  if (seriesName == null) return null;
  if (series.type === "none") return null;

  const seriesConfig = config?.[seriesName];

  const color = seriesConfig?.color ?? series.color;
  const canToggleSeriesVisibility = toggleSeriesVisibility != null;
  const otherSeriesActive = activeSeries != null && activeSeries !== seriesName;
  const isHidden = hiddenSeries?.has(seriesName) ?? false;

  const setSeriesActive = (active: boolean) => {
    if (isHidden) return;
    if (active) {
      setActiveSeries(seriesName);
    } else {
      setActiveSeries(curr => (curr === seriesName ? undefined : curr));
    }
  };

  const handleClick = (e: React.MouseEvent) => {
    if (!canToggleSeriesVisibility) return;

    toggleSeriesVisibility(seriesName, e.metaKey || e.ctrlKey);
    // Clear the active (hovered) series so other items aren't left dimmed after clicking
    setActiveSeries(undefined);
  };

  const name = seriesConfig?.label ?? String(seriesName);
  const displayName = truncateText(name, LEGEND_TEXT_MAX_LENGTH);
  const tooltipContent = seriesConfig?.tooltipContent;

  const legendItem = (
    <div
      className={cn("flex max-w-full items-center gap-2 px-2 py-0.5", {
        "cursor-pointer": canToggleSeriesVisibility,
        "opacity-30": !isHidden && otherSeriesActive,
        "opacity-50": isHidden,
      })}
      onClick={canToggleSeriesVisibility ? handleClick : undefined}
      onMouseEnter={() => setSeriesActive(true)}
      onMouseLeave={() => setSeriesActive(false)}
    >
      {color != null && (
        <div
          className="size-2.5 shrink-0 rounded-sm bg-[--color-bg]"
          style={{ "--color-bg": color, "--color-border": color } as React.CSSProperties}
        />
      )}

      <span className="truncate" title={name}>
        {seriesConfig?.labelNode ?? displayName}
      </span>
    </div>
  );

  if (tooltipContent != null) {
    return (
      <Tooltip content={tooltipContent} interactiveChild>
        {legendItem}
      </Tooltip>
    );
  }

  return legendItem;
}
