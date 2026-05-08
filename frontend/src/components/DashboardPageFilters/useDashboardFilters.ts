"use client";

import { useCallback, useMemo, useState } from "react";

export type Granularity = "hour" | "day";

export interface DateRange {
  /** Stable key used in the dropdown and persisted state. */
  id: string;
  label: string;
  days: number;
  /** Default granularity if the user hasn't picked one explicitly. */
  defaultGranularity: Granularity;
}

export const DATE_RANGES: readonly DateRange[] = [
  { id: "24h", label: "Last 24 hours", days: 1, defaultGranularity: "hour" },
  { id: "7d", label: "Last 7 days", days: 7, defaultGranularity: "day" },
  { id: "30d", label: "Last 30 days", days: 30, defaultGranularity: "day" },
  { id: "90d", label: "Last 90 days", days: 90, defaultGranularity: "day" },
] as const;

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
export function useDashboardFilters(initialRangeId: string = "30d"): UseDashboardFiltersResult {
  const [rangeId, setRangeIdState] = useState<string>(initialRangeId);
  const [granularityOverride, setGranularityOverride] = useState<Granularity | null>(null);

  const range = useMemo(
    () => DATE_RANGES.find(r => r.id === rangeId) ?? DATE_RANGES[2],
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

  // Compute date window. Fresh Date objects on each call so timeseries
  // requests stay aligned with the wall clock; React.useMemo keys on
  // rangeId so we don't recompute on unrelated state changes.
  const { fromISO, toISO } = useMemo(() => {
    const to = new Date();
    const from = new Date(to);
    from.setDate(from.getDate() - range.days);
    return { fromISO: from.toISOString(), toISO: to.toISOString() };
  }, [range.days]);

  return {
    filters: { range, granularity, fromISO, toISO },
    setRangeId,
    setGranularity,
  };
}
