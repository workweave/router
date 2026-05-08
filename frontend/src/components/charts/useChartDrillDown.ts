"use client";

import { type DateTime } from "@/objects/scalars/DateTime";
import { useCallback, useState } from "react";

export type ChartGranularity = "hour" | "day" | "week";

interface DrillDownState {
  fromISO: string;
  toISO: string;
  title: string;
}

/** Returns the [from, to) ISO bounds for a chart bucket of the given granularity. */
export function bucketBounds(time: DateTime, granularity: ChartGranularity): [string, string] {
  const start = new Date(time);
  const end = new Date(start);
  switch (granularity) {
    case "hour":
      end.setHours(end.getHours() + 1);
      break;
    case "day":
      end.setDate(end.getDate() + 1);
      break;
    case "week":
      end.setDate(end.getDate() + 7);
      break;
  }
  return [start.toISOString(), end.toISOString()];
}

function formatBucketTitle(time: DateTime, granularity: ChartGranularity): string {
  const d = new Date(time);
  if (granularity === "week") {
    const end = new Date(d);
    end.setDate(end.getDate() + 6);
    const startStr = d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
    const endStr = end.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
    return `${startStr} – ${endStr}`;
  }
  if (granularity === "day") {
    return d.toLocaleDateString(undefined, {
      weekday: "short",
      month: "short",
      day: "numeric",
      year: "numeric",
    });
  }
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/**
 * Convenience hook: tracks the drill-down dialog open state and the active
 * [from, to) bucket window. Each chart calls `open(time)` from its
 * `onClickDataPoint` handler; the page renders the modal driven by the
 * returned state.
 */
export function useChartDrillDown(granularity: ChartGranularity) {
  const [state, setState] = useState<DrillDownState | null>(null);

  const open = useCallback(
    (time: DateTime) => {
      const [fromISO, toISO] = bucketBounds(time, granularity);
      setState({ fromISO, toISO, title: formatBucketTitle(time, granularity) });
    },
    [granularity],
  );

  const close = useCallback(() => setState(null), []);

  return { state, open, close, isOpen: state != null };
}
