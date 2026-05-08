import { DateTime } from "@/objects/scalars/DateTime";

/**
 * Parses a chart data value that may be a date string and converts it to a timestamp.
 *
 * Used for inline data charts where the LLM provides date strings like "2025-09-23"
 * that need to be converted to numeric timestamps for Recharts time axes.
 *
 * Note: Uses direct Date parsing (UTC) rather than DateScalar.toDate() to avoid
 * timezone adjustments that could shift chart data points.
 *
 * @param value - The value to parse (could be number, date string, or other)
 * @returns Numeric timestamp if parseable as date, otherwise the original value as string
 */
export function parseChartTimeValue(value: unknown): number | string {
  if (typeof value === "number") return value;

  if (typeof value === "string") {
    const date = new Date(value);
    if (!isNaN(date.getTime())) {
      return date.getTime() as DateTime;
    }
  }

  return String(value);
}
