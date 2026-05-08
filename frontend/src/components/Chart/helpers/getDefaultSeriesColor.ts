import { ChartColor } from "../color";

/**
 * Default color palette for chart series. Colors are ordered for maximum
 * visual distinction between adjacent series.
 *
 * Exported so that series-config hooks (accounts, teams, projects) can assign
 * explicit colors by index — this is how we guarantee the same entity gets
 * the same color on the master chart, pie chart, drill-downs, etc., even when
 * those surfaces render series in different orders.
 */
export const SERIES_COLORS: readonly string[] = [
  ChartColor.Scale1, // #FF6C47 - orange-red
  ChartColor.Scale2, // #1D5C83 - dark blue
  ChartColor.Green, // #3FB950 - green
  ChartColor.GoalLine, // #7b70c9 - purple
  ChartColor.Orange2, // #EC9C41 - amber
  ChartColor.Benchmark1, // #41CAEC - cyan
  ChartColor.Red, // #DA3633 - red
  ChartColor.Benchmark3, // #009998 - teal
  ChartColor.BenchmarkP90, // #F7CD04 - yellow
  ChartColor.Scale3, // #358CC2 - medium blue
];

/**
 * Gets a default color, based on the index of the series in the data.
 */
export function getDefaultSeriesColor(seriesIndex: number): string {
  return SERIES_COLORS[seriesIndex % SERIES_COLORS.length];
}

const PIE_SERIES_COLORS = [
  ChartColor.Scale1,
  ChartColor.Scale2,
  ChartColor.Scale3,
  ChartColor.Scale4,
  ChartColor.Scale5,
];

/**
 * Gets a default color, based on the index of the series in the data, for a pie chart.
 */
export function getDefaultPieChartSeriesColor(seriesIndex: number): string {
  return PIE_SERIES_COLORS[seriesIndex % PIE_SERIES_COLORS.length];
}
