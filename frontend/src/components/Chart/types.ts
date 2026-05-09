import { TOOLTIP_DATA_KEY } from "./constants";

/** Allowable types for chart data keys. */
export type ChartDataKeyType = number | string;

/** Allowable types for chart data values. */
export type ChartDataValueType = [number, number] | [string, string] | number | string;

/**
 * Represents a fixed value for a chart's axis domain.
 */
type ChartAxisDomainItem<TValue> = "auto" | "dataMax" | "dataMin" | TValue;

/**
 * Function to get the domain for an axis. This function is called with the min and max data values
 * passed by the chart, and should return the domain to use for the axis.
 */
type ChartAxisDomainGetter<TValue> = (
  [dataMin, dataMax]: [TValue, TValue],
  allowDataOverflow: boolean,
) => [TValue, TValue];

/**
 * The domain for an axis in a chart. This can be a pair of values, or a function that generates the
 * domain.
 */
export type ChartAxisDomain<TValue> =
  | [ChartAxisDomainItem<TValue>, ChartAxisDomainItem<TValue>]
  | ChartAxisDomainGetter<TValue>;

/**
 * The type of data on an axis in a chart.
 */
export enum ChartAxisType {
  Category = "category",
  Number = "number",
  Time = "time",
}

/**
 * The dependent axis to display a series on.
 */
export enum ChartDependentAxis {
  Left = "left",
  Right = "right",
}

/**
 * Defines a line on an XY chart.
 */
export interface ChartLineDefinition {
  /** The y-intercept of the line. */
  intercept: number;
  /** The slope of the line. */
  slope: number;
}

/**
 * Different ways a chart series can be displayed.
 */
export enum ChartSeriesType {
  Area = "area",
  Bar = "bar",
  Line = "line",
}

/**
 * Configuration for a single chart series. Note that not every chart type will support all
 * properties.
 */
export interface ChartSeriesConfig<
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
> {
  /**
   * The color to use for this series in the chart. If not set, defaults to an automatic color that
   * will be different from adjacent series as much as possible.
   */
  color?: string;
  /**
   * If this series is a line and this is true, the line will be drawn to connect null values.
   *
   * @default false
   */
  connectNulls?: boolean;
  /**
   * If true, the series will be displayed as a dashed line/bar.
   *
   * @default false
   */
  dashed?: boolean;
  /**
   * The axis to display this series on.
   *
   * @default ChartDependentAxis.Left
   */
  dependentAxis?: ChartDependentAxis;
  /**
   * If true, the series will have a dot when it is displayed as a line. Has no effect if the series
   * is displayed as a bar.
   *
   * @default true
   */
  dot?: boolean;
  /**
   * If provided, specially formats values for this series only. Only applies to the series tooltip,
   * not to axis labels (prefer `formatValue` on the chart props for that).
   */
  formatValue?: (value: TDependentValue) => string;
  /**
   * The icon to display in the legend and tooltip for this series.
   *
   * Defaults to a circle with the series color.
   */
  icon?: React.ReactNode;
  /**
   * The opacity to use for this series when it is inactive.
   *
   * @default 0.3
   */
  inactiveOpacity?: number;
  /**
   * If true, the user can interact with the series.
   *
   * @default true
   */
  interactive?: boolean;
  /**
   * The name for the series.
   *
   * @example "PRs merged"
   */
  label?: string;
  /**
   * Rich-content override for the legend/tooltip label. When provided, takes precedence over
   * `label` for rendering, while `label` is still used as the plain-text fallback (e.g. for
   * `title` attributes and truncation calculations).
   */
  labelNode?: React.ReactNode;
  /**
   * The layer to display this series in. Use this if you want to e.g. display two bars on top of
   * one another, not stacked.
   *
   * Series in higher layers are rendered **under** series in lower layers.
   *
   * @default 0
   */
  layer?: 0 | 1;
  /**
   * Whether to display this series in the legend.
   *
   * @default true
   */
  legend?: boolean;
  /**
   * The opacity to use for this series in the chart.
   *
   * @default 1
   */
  opacity?: number;
  /**
   * Whether to display this series in the tooltip.
   *
   * @default true
   */
  showTooltip?: ((key: TIndependentValue) => boolean) | boolean;
  /**
   * Content to display in the tooltip for this series in the legend.
   */
  tooltipContent?: React.ReactNode;
  /**
   * How the series should be displayed.
   *
   * @default ChartSeriesType.Bar
   */
  type?: ChartSeriesType;
}

/**
 * Configuration for a single chart, with an optional configuration for each series in the chart.
 */
export type ChartConfig<
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
> = Readonly<Partial<Record<TDependentKey, ChartSeriesConfig<TIndependentValue, TDependentValue>>>>;

/**
 * The dependent values for a single data point in a chart.
 */
export type ChartDataPointDependentValues<
  TDependentKey extends ChartDataKeyType,
  TDependentValue extends ChartDataValueType,
> = Partial<Readonly<Record<TDependentKey, TDependentValue | null>>>;

/**
 * A chart data point is a single data point in a chart, which always contains the independent value
 * and may contain data for any of the dependent series in the chart.
 */
export type ChartDataPoint<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
  TTooltipData = unknown,
> = Readonly<Record<TIndependentKey, TIndependentValue>> &
  ChartDataPointDependentValues<TDependentKey, TDependentValue> & {
    [TOOLTIP_DATA_KEY]?: TTooltipData;
  };

/**
 * Chart data is represented as an array of data points, one data point per independent value.
 */
export type ChartData<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
  TTooltipData = unknown,
> = ChartDataPoint<
  TIndependentKey,
  TDependentKey,
  TIndependentValue,
  TDependentValue,
  TTooltipData
>[];

interface ChartReferenceLineCommon {
  /** The color of the reference line (CSS color string). */
  color?: string;
  /** If true, the reference line will be drawn as a dashed line. */
  dashed?: boolean;
  /** The label to display for the reference line. */
  label?: string;
  /**
   * The side of the line to put the label on.
   *
   * @default "center"
   */
  side?: "center" | "left" | "right";
}

/**
 * A reference line to draw on a chart at a given independent value.
 */
export interface ChartIndependentReferenceLine<TIndependentValue extends ChartDataValueType>
  extends ChartReferenceLineCommon {
  type: "independent";
  /** The value to draw the reference line at. */
  value: TIndependentValue;
}

/**
 * A reference line to draw on a chart at a given dependent value.
 */
export interface ChartDependentReferenceLine<TDependentValue extends ChartDataValueType>
  extends ChartReferenceLineCommon {
  type: "dependent";
  /** The value to draw the reference line at. */
  value: TDependentValue;
}

export type ChartReferenceLine<
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
> = ChartDependentReferenceLine<TDependentValue> | ChartIndependentReferenceLine<TIndependentValue>;
