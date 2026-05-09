import { X_AXIS_TICK_MARGIN } from "./constants";
import { ChartDataValueType } from "./types";

export interface ChartTickProps<TIndependentValue extends ChartDataValueType> {
  height?: number | string;
  index?: number;
  payload?: { value?: unknown };
  renderTick: (value: TIndependentValue) => React.ReactNode;
  visibleTicksCount?: number;
  width?: number | string;
  x?: number | string;
  y?: number | string;
}

/**
 * Renders a custom tick for the x-axis of a chart.
 */
export function ChartTick<TIndependentValue extends ChartDataValueType>({
  height,
  index,
  payload,
  renderTick,
  visibleTicksCount,
  width,
  x,
  y,
}: ChartTickProps<TIndependentValue>) {
  if (payload == null) return null;
  if (payload.value == null) return null;
  if (typeof x !== "number") return null;
  if (typeof y !== "number") return null;
  if (typeof height !== "number") return null;
  if (typeof width !== "number") return null;
  if (typeof visibleTicksCount !== "number") return null;
  if (typeof index !== "number") return null;

  const tickWidth = width / visibleTicksCount;
  const leftOffset = x - tickWidth / 2;
  const adjustedHeight = height - X_AXIS_TICK_MARGIN - 1; // 1px for the line

  return (
    <foreignObject x={leftOffset} y={y} height={adjustedHeight} width={tickWidth}>
      {renderTick(payload.value as TIndependentValue)}
    </foreignObject>
  );
}
