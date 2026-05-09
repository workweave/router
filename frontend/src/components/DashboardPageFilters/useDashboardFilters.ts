"use client";

import { useCallback, useMemo, useState } from "react";

export type Granularity = "hour" | "day" | "week";

export interface DateRange {
  /** Stable key used in the dropdown and persisted state. */
  id: string;
  label: string;
  /** Default granularity if the user hasn't picked one explicitly. */
  defaultGranularity: Granularity;
  /** Computes the range start from the current `now`. */
  start: (now: Date) => Date;
}

/** Start of the ISO week (Monday) containing `d`, at 00:00 local. */
function startOfWeek(d: Date): Date {
  const out = new Date(d);
  out.setHours(0, 0, 0, 0);
  // getDay: 0=Sun ... 6=Sat. Shift so Mon=0.
  const day = (out.getDay() + 6) % 7;
  out.setDate(out.getDate() - day);
  return out;
}

/** Start of the month containing `d`, at 00:00 local. */
function startOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), 1, 0, 0, 0, 0);
}

function addWeeks(d: Date, n: number): Date {
  const out = new Date(d);
  out.setDate(out.getDate() + n * 7);
  return out;
}

function addMonths(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth() + n, d.getDate(), 0, 0, 0, 0);
}

/**
 * Mirrors WorkWeave's default date filter set (week- and month-anchored
 * windows ending at "now"), trimmed to the granularities the router
 * supports.
 */
export const DATE_RANGES: readonly DateRange[] = [
  {
    id: "this-week",
    label: "This week",
    defaultGranularity: "day",
    start: now => startOfWeek(now),
  },
  {
    id: "last-month",
    label: "Last month",
    defaultGranularity: "day",
    start: now => addWeeks(startOfWeek(now), -4),
  },
  {
    id: "last-2-months",
    label: "Last 2 months",
    defaultGranularity: "day",
    start: now => addWeeks(startOfWeek(now), -8),
  },
  {
    id: "last-3-months",
    label: "Last 3 months",
    defaultGranularity: "day",
    start: now => addWeeks(startOfWeek(now), -12),
  },
  {
    id: "last-5-months",
    label: "Last 5 months",
    defaultGranularity: "day",
    start: now => addMonths(startOfMonth(now), -5),
  },
  {
    id: "last-11-months",
    label: "Last 11 months",
    defaultGranularity: "day",
    start: now => addMonths(startOfMonth(now), -11),
  },
] as const;

const DEFAULT_RANGE_ID = "last-month";

export interface DashboardFilters {
  range: DateRange;
  granularity: Granularity;
  /** ISO timestamp of the range start. */
  fromISO: string;
  /** ISO timestamp of the range end (now). */
  toISO: string;
}

export interface UseDashboardFiltersResult {
  filters: DashboardFilters;
  setRangeId: (id: string) => void;
  setGranularity: (g: Granularity) => void;
}

/**
 * Router-local filter state. Mirrors WW's useDashboardPageFilters /
 * useGranularitySelector contract but without nuqs/PostHog/GraphQL.
 * Granularity defaults from the selected range; the user can override
 * it with setGranularity, and the override is reset whenever the range
 * changes.
 */
export function useDashboardFilters(
  initialRangeId: string = DEFAULT_RANGE_ID,
): UseDashboardFiltersResult {
  const [rangeId, setRangeIdState] = useState<string>(initialRangeId);
  const [granularityOverride, setGranularityOverride] = useState<Granularity | null>(null);

  const range = useMemo(
    () =>
      DATE_RANGES.find(r => r.id === rangeId) ??
      DATE_RANGES.find(r => r.id === DEFAULT_RANGE_ID) ??
      DATE_RANGES[0],
    [rangeId],
  );
  const granularity = granularityOverride ?? range.defaultGranularity;

  const setRangeId = useCallback((id: string) => {
    setRangeIdState(id);
    setGranularityOverride(null);
  }, []);

  const setGranularity = useCallback((g: Granularity) => {
    setGranularityOverride(g);
  }, []);

  const { fromISO, toISO } = useMemo(() => {
    const to = new Date();
    const from = range.start(to);
    return { fromISO: from.toISOString(), toISO: to.toISOString() };
  }, [range]);

  return {
    filters: { range, granularity, fromISO, toISO },
    setRangeId,
    setGranularity,
  };
}
