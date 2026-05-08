import { ChartColor } from "../color";

const SERIES_COLORS: readonly string[] = [
  ChartColor.Scale1,
  ChartColor.Scale2,
  ChartColor.Green,
  ChartColor.GoalLine,
  ChartColor.Orange2,
  ChartColor.Benchmark1,
  ChartColor.Red,
  ChartColor.Benchmark3,
  ChartColor.BenchmarkP90,
  ChartColor.Scale3,
];

export function getDefaultSeriesColor(seriesIndex: number): string {
  return SERIES_COLORS[seriesIndex % SERIES_COLORS.length];
}
