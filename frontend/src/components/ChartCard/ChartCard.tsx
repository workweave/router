import { Text } from "@/components/atoms/Text";
import { Card } from "@/components/molecules/Card";
import { cn } from "@/tools/cn";

import React from "react";


export interface ChartCardProps extends React.PropsWithChildren {
  /**
   * Stable id used for deep linking. When provided, the card is marked with a
   * `data-chart-id` attribute and an ellipsis menu (with "Copy link to chart")
   * is rendered in the hover action row.
   */
  chartId?: string;
  className?: string;
  /**
   * Optional "Explain with AI" action, rendered as a row inside the kebab
   * menu. Only used when {@link chartId} is provided.
   */
  explainAction?: React.ReactNode;
  /**
   * If provided, renders full-width content below the title/topRight row within the header.
   */
  headerBottom?: React.ReactNode;
  /**
   * True if the chart has empty data (note this is different from when it is still loading).
   *
   * @default false
   */
  isEmpty?: boolean;
  /**
   * Additional items to render in the kebab menu (after the built-in "Copy
   * link" item). Only rendered when {@link chartId} is provided.
   */
  menuExtraItems?: React.ReactNode;
  /**
   * Slot to render an optional overlay over the chart.
   */
  overlay?: React.ReactNode;
  /**
   * Optional chart-specific settings body (e.g., previous-period toggle +
   * goal-line editor). When provided, the kebab menu adds a "Chart settings"
   * row that navigates to a sub-view containing this content. Only used when
   * {@link chartId} is provided.
   */
  settingsContent?: React.ReactNode;
  /**
   * If provided, renders the sources for the metric next to the title.
   */
  sources?: React.ReactNode;
  /**
   * The optional subtitle to show in the header.
   */
  subtitle?: React.ReactNode;
  /**
   * The chart's title.
   */
  title: React.ReactNode;
  /**
   * If provided, renders content in the top right corner of the card.
   */
  topRight?: React.ReactNode;
  /**
   * If true, renders the title as a single-line truncating element with
   * ellipsis + full-text hover tooltip. Set to false only when rendering a
   * composite title block (e.g. a dropdown or multi-row title) that manages
   * its own layout — the caller is then responsible for any needed
   * truncation inside that block.
   *
   * @default true
   */
  truncateTitle?: boolean;
}

/**
 * Renders a card that contains a chart, with some common styling. Does not have any opinions about
 * the chart itself.
 *
 * Chart-level actions ("Explain with AI", "Chart settings") live inside the
 * kebab menu (see {@link ChartMenuSlot}), which slides in to the left of the
 * `topRight` slot on card hover. The `topRight` slot stays anchored to the
 * right edge so any clickable content there doesn't shift while the kebab
 * appears.
 */
export function ChartCard({
  chartId,
  children,
  className,
  explainAction,
  headerBottom,
  isEmpty = false,
  menuExtraItems,
  overlay,
  settingsContent,
  sources,
  subtitle,
  title,
  topRight,
  truncateTitle = true,
}: ChartCardProps) {
  return (
    <Card
      className={cn(
        "relative h-full w-full shrink grow p-2 @container md:p-4",
        { "opacity-80": isEmpty },
        className,
      )}
      data-chart-id={chartId}
    >
      <Card.Header>
        <div className="group/chart-header flex flex-row items-start gap-4">
          <div className="min-w-0 grow overflow-hidden">
            <Card.Title
              variant="h4"
              className="flex min-h-8 max-w-full grow items-center gap-3 leading-tight"
            >
              {truncateTitle ?
                <span className="truncate" title={typeof title === "string" ? title : undefined}>
                  {title}
                </span>
              : title}
              {sources != null && <div className="hidden shrink-0 @sm:block">{sources}</div>}
            </Card.Title>

            {React.Children.count(subtitle) > 0 && (
              <Text
                variant="p"
                className="truncate text-sm text-muted-foreground"
                title={typeof subtitle === "string" ? subtitle : undefined}
              >
                {subtitle}
              </Text>
            )}
          </div>

          <div className="flex shrink-0 items-start">
            
            {topRight}
          </div>
        </div>
        {headerBottom}
      </Card.Header>

      <Card.Content className="relative h-80 min-h-0 grow overflow-hidden">{children}</Card.Content>

      {overlay}
    </Card>
  );
}
