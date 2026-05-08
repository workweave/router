import { ChartColor } from "../color";
import { ChartDataValueType, ChartSeriesConfig } from "../types";

/**
 * Prefix used for zero-value series keys to distinguish them from regular series
 */
const ZERO_VALUE_PREFIX = "_zero_";

/**
 * Configuration for zero-value bar display
 */
export interface ZeroValueBarConfig {
  /**
   * Percentage of max value to use as the height for zero bars (e.g., 0.05 for 5%)
   * @default 0.05
   */
  heightPercentage?: number;

  /**
   * Custom label format for zero-value series.
   * If not provided, defaults to "Ignored ({label})"
   */
  getLabelFormat?: (originalLabel: string) => string;

  /**
   * Whether to show zero-value series in the legend
   * @default false
   */
  showInLegend?: boolean;

  /**
   * Custom color for zero-value bars.
   * Can be a function that returns a color based on the series key.
   * If not provided, uses the original series color with dashed style.
   */
  getColor?: (seriesKey: string) => ChartColor | undefined;
}

/**
 * Generates a zero-value series key from a regular series key
 */
export function getZeroValueSeriesKey<TKey extends string>(key: TKey): string {
  return `${ZERO_VALUE_PREFIX}${key}`;
}

/**
 * Checks if a series key is a zero-value series key
 */
export function isZeroValueSeriesKey(key: string): boolean {
  return key.startsWith(ZERO_VALUE_PREFIX);
}

/**
 * Extracts the original series key from a zero-value series key
 */
export function getOriginalSeriesKey(zeroKey: string): string {
  if (!isZeroValueSeriesKey(zeroKey)) {
    return zeroKey;
  }
  return zeroKey.slice(ZERO_VALUE_PREFIX.length);
}

/**
 * Creates zero-value series configurations based on regular series configs
 */
export function createZeroValueSeriesConfigs<
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
>(
  regularConfigs: Record<string, ChartSeriesConfig<TIndependentValue, TDependentValue>>,
  options: ZeroValueBarConfig = {},
): Record<string, ChartSeriesConfig<TIndependentValue, TDependentValue>> {
  const {
    getColor,
    getLabelFormat = label => `Ignored (${label.toLowerCase()})`,
    showInLegend = false,
  } = options;

  const zeroConfigs: Record<string, ChartSeriesConfig<TIndependentValue, TDependentValue>> = {};

  for (const [key, config] of Object.entries(regularConfigs)) {
    const zeroKey = getZeroValueSeriesKey(key);
    const label = config.label ?? key;
    const customColor = getColor?.(key);

    zeroConfigs[zeroKey] = {
      ...config,
      dashed: true, // Diagonal stripes to indicate zero/ignored
      formatValue: () => "", // Hide numeric value in tooltip
      label: getLabelFormat(label),
      legend: showInLegend,
      ...(customColor != null && { color: customColor }),
    };
  }

  return zeroConfigs;
}

/**
 * Result from processing data with zero values
 */
export interface ProcessedZeroValueData<TDataPoint> {
  data: TDataPoint[];
  hasZeroValues: boolean;
  minVisibleValue: number;
}

/**
 * Processes chart data to add zero-value series for entries that have data but zero values
 *
 * @param dataPoints Array of data points to process
 * @param getValue Function to get the value for a given key from a data point
 * @param setValue Function to set a value for a given key in a data point
 * @param hasData Function to check if data exists for a given key (true = has data but might be zero)
 * @param keys The series keys to check
 * @param maxValue The maximum value in the dataset (used to calculate bar height)
 * @param options Configuration options
 */
export function processDataWithZeroValues<
  TDataPoint extends Record<string, unknown>,
  TKey extends string,
>(
  dataPoints: TDataPoint[],
  getValue: (dataPoint: TDataPoint, key: TKey) => number | undefined,
  setValue: (dataPoint: TDataPoint, key: string | TKey, value: number) => void,
  hasData: (dataPoint: TDataPoint, key: TKey) => boolean,
  keys: readonly TKey[],
  maxValue: number,
  options: ZeroValueBarConfig = {},
): ProcessedZeroValueData<TDataPoint> {
  const { heightPercentage = 0.05 } = options;
  const minVisibleValue = maxValue > 0 ? maxValue * heightPercentage : 1;

  let hasZeroValues = false;

  const processedData = dataPoints.map(dataPoint => {
    const newDataPoint = { ...dataPoint };

    for (const key of keys) {
      if (hasData(dataPoint, key)) {
        const value = getValue(dataPoint, key) ?? 0;

        if (value > 0) {
          // Regular value, keep as is
          setValue(newDataPoint, key, value);
        } else {
          // Has data but value is zero (ignored/overridden)
          const zeroKey = getZeroValueSeriesKey(key);
          setValue(newDataPoint, zeroKey, minVisibleValue);
          hasZeroValues = true;
        }
      }
    }

    return newDataPoint;
  });

  return {
    data: processedData,
    hasZeroValues,
    minVisibleValue,
  };
}

/**
 * Helper to handle drill-down clicks for both regular and zero-value series
 * Maps zero-value keys back to their original keys
 */
export function handleZeroValueDrillDown<TKey extends string>(
  clickedKey: string,
  callback: (originalKey: TKey) => void,
): void {
  const originalKey = getOriginalSeriesKey(clickedKey) as TKey;
  callback(originalKey);
}

/**
 * Extends dependent keys array to include zero-value series keys
 */
export function extendKeysWithZeroValues<TKey extends string>(
  keys: readonly TKey[],
): readonly (string | TKey)[] {
  const zeroKeys = keys.map(key => getZeroValueSeriesKey(key));
  return [...keys, ...zeroKeys];
}

/**
 * Creates a mapping function for use in chart click handlers that automatically
 * handles zero-value series key mapping
 */
export function createZeroValueClickHandler<
  TIndependentValue extends ChartDataValueType,
  TDependentKey extends string,
>(onClickDataPoint: (value: TIndependentValue, seriesKey: TDependentKey) => void) {
  return (value: TIndependentValue, clickedKey: string | TDependentKey) => {
    const originalKey = getOriginalSeriesKey(clickedKey as string) as TDependentKey;
    onClickDataPoint(value, originalKey);
  };
}
