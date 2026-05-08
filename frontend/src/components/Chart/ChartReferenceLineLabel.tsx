import { ChartReferenceLine } from "./types";

const LABEL_OFFSET = 4;

export interface ChartReferenceLineLabelProps
  extends Pick<ChartReferenceLine<number, number>, "side"> {
  label?: string;
  viewBox?: { height?: number; width?: number; x?: number; y?: number };
}

const TEXT_ANCHOR: Readonly<
  Record<Required<ChartReferenceLine<number, number>>["side"], "end" | "middle" | "start">
> = {
  center: "middle",
  left: "end",
  right: "start",
};

/**
 * Renders a label for a reference line.
 */
export function ChartReferenceLineLabel({
  label,
  side = "center",
  viewBox,
}: ChartReferenceLineLabelProps) {
  if (label == null) return null;

  let x = viewBox?.x;
  if (x != null && side === "left") {
    x = x - LABEL_OFFSET;
  } else if (x != null && side === "right") {
    x = x + LABEL_OFFSET;
  }

  return (
    <text x={x} y={-8} className="fill-muted-foreground px-1" textAnchor={TEXT_ANCHOR[side]}>
      {label}
    </text>
  );
}
