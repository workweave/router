import { cn } from "@/tools/cn";

import { forwardRef, useId } from "react";

export interface ChartContainerProps extends React.HTMLAttributes<HTMLDivElement> {}

/**
 * Renders a container for a chart. This component handles styling.
 */
export const ChartContainer = forwardRef<HTMLDivElement, ChartContainerProps>(
  ({ children, className, id, ...props }, ref) => {
    const uniqueId = useId();
    const chartId = `chart-${id ?? uniqueId.replace(/:/g, "")}`;

    return (
      <div
        data-chart={chartId}
        ref={ref}
        className={cn(
          "relative flex aspect-auto h-full min-h-48 w-full min-w-56 justify-center text-xs [&_.recharts-cartesian-axis-tick_text]:fill-muted-foreground [&_.recharts-cartesian-grid_line[stroke='#ccc']]:stroke-border/50 [&_.recharts-curve.recharts-tooltip-cursor]:stroke-border [&_.recharts-dot[stroke='#fff']]:stroke-transparent [&_.recharts-layer]:outline-none [&_g[tabindex]]:outline-none [&_.recharts-polar-grid_[stroke='#ccc']]:stroke-border [&_.recharts-radial-bar-background-sector]:fill-muted [&_.recharts-rectangle.recharts-tooltip-cursor]:fill-muted [&_.recharts-reference-line_[stroke='#ccc']]:stroke-border [&_.recharts-sector[stroke='#fff']]:stroke-transparent [&_.recharts-sector]:outline-none [&_.recharts-surface:focus-visible]:ring-1 [&_.recharts-surface:has(.recharts-layer.recharts-pie:focus-visible)]:ring-1 [&_.recharts-surface]:rounded [&_.recharts-surface]:outline-none [&_.recharts-surface]:ring-primary [&_.recharts-surface]:ring-offset-4",
          className,
        )}
        {...props}
      >
        {children}
      </div>
    );
  },
);
ChartContainer.displayName = "ChartContainer";
