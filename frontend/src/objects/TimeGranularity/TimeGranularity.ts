import { addDays, addWeeks } from "date-fns";
import { formatInTimeZone } from "date-fns-tz";

const UTC = "UTC";
const YEAR_MONTH_DAY = "yyyy-MM-dd";

export enum TimeGranularity {
  Day = "day",
  Week = "week",
}

export function addGranularity(
  time: Date | number,
  granularity: TimeGranularity,
  amount: number,
): Date {
  return granularity === TimeGranularity.Week ? addWeeks(time, amount) : addDays(time, amount);
}

export function formatTime(time: Date | number, _granularity: TimeGranularity): string {
  return formatInTimeZone(time, UTC, YEAR_MONTH_DAY);
}

export function formatInterval(time: Date | number, granularity: TimeGranularity): string {
  if (granularity === TimeGranularity.Week) {
    const end = addDays(addWeeks(time, 1), -1);
    return `${formatInTimeZone(time, UTC, YEAR_MONTH_DAY)} to ${formatInTimeZone(end, UTC, YEAR_MONTH_DAY)}`;
  }
  return formatInTimeZone(time, UTC, YEAR_MONTH_DAY);
}
