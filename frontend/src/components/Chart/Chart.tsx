import { LoadState } from "@/tools/LoadState";
import { groupBy } from "@/tools/map";
import { SafeOmit, unreachable } from "@/tools/types";

import React, { useEffect, useMemo, useRef, useState } from "react";
import * as Recharts from "recharts";

import { ChartContainer } from "./ChartContainer";
import { getChartContext } from "./ChartContext";
import { ChartLegend } from "./ChartLegend";
import { ChartMessageCard } from "./ChartMessageCard";
import { ChartReferenceLineLabel } from "./ChartReferenceLineLabel";
import { ChartTick } from "./ChartTick";
import { ChartTooltip } from "./ChartTooltip";
import { X_AXIS_TICK_MARGIN } from "./constants";
import { getDefaultSeriesColor } from "./helpers/getDefaultSeriesColor";
import { getRechartsDomain } from "./helpers/getRechartsDomain";
import {
  ChartAxisDomain,
  ChartAxisType,
  ChartConfig,
  ChartData,
  ChartDataKeyType,
  ChartDataValueType,
  ChartDependentAxis,
  ChartReferenceLine,
  ChartSeriesType,
} from "./types";
import { useToggleSeriesVisibility } from "./useToggleSeriesVisibility";

const ANIMATION_DURATION = 400;
const STROKE_WIDTH = 2;
const MIN_BAR_HEIGHT_PX = 3;

/**
 * Custom bar shape that enforces a minimum pixel height for non-zero values.
 *
 * Recharts' built-in `minPointSize` doesn't work for stacked bars because it receives
 * the cumulative stack value, not the individual bar's contribution. A 0-value bar
 * stacked on a 1000-value bar gets value[1]=1000, so minPointSize treats it as non-zero.
 *
 * This shape computes the actual bar contribution (value[1] - value[0]) and only
 * enforces the minimum height when the contribution is genuinely > 0.
 */
interface MinHeightBarProps extends Recharts.RectangleProps {
  value?: [number, number] | number;
}

function MinHeightBar(props: MinHeightBarProps) {
  const { height = 0, value, y = 0, ...rest } = props;

  const barContribution =
    Array.isArray(value) ? Math.abs(value[1] - value[0])
    : typeof value === "number" ? Math.abs(value)
    : 0;

  if (barContribution === 0) {
    return <Recharts.Rectangle {...rest} height={0} y={y} />;
  }

  if (Math.abs(height) >= MIN_BAR_HEIGHT_PX) {
    return <Recharts.Rectangle {...rest} height={height} y={y} />;
  }

  const sign = height >= 0 ? 1 : -1;
  const delta = MIN_BAR_HEIGHT_PX - Math.abs(height);
  return <Recharts.Rectangle {...rest} height={sign * MIN_BAR_HEIGHT_PX} y={y - sign * delta} />;
}

export interface ChartProps<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
  TTooltipData = unknown,
> extends SafeOmit<React.HTMLAttributes<HTMLDivElement>, "children"> {
  /**
   * The data for the chart. See the {@link ChartData} type for more.
   */
  data: LoadState<ChartData<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>>;
  /**
   * The keys in the chart data for the dependent values. The order of these keys determines the
   * order of the series in the chart.
   */
  dependentKeys: LoadState<readonly TDependentKey[]>;
  /**
   * The key in the chart data for the independent value. For example, in a time series chart, this
   * could be `"time"`.
   */
  independentKey: TIndependentKey;

  /**
   * Allow the ticks of the dependent axis to be decimals.
   *
   * @default false
   */
  allowDependentTickDecimals?: boolean;
  /**
   * Configuration for specific chart series.
   */
  config?: ChartConfig<TDependentKey, TIndependentValue, TDependentValue>;
  /**
   * A ref to pass to the container.
   */
  containerRef?: React.Ref<HTMLDivElement>;
  /**
   * Label for the dependent (Y) axis.
   */
  dependentAxisLabel?: string;
  /**
   * The domain of the dependent axis. If not provided, the domain will be automatically calculated.
   */
  dependentDomain?: ChartAxisDomain<TDependentValue>;
  /**
   * The type of the dependent values.
   *
   * @default ChartAxisType.Number
   */
  dependentType?: ChartAxisType;
  /**
   * If provided, this data will be shown when the chart has no data. This can be useful to ensure
   * the chart doesn't jump around too much when data is loading.
   */
  emptyData?: ChartData<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>;
  /**
   * The message to show when the chart has no data.
   *
   * @default
   * <Card.Header className="text-center">
   *   <Card.Title>No data yet</Card.Title>
   * </Card.Header>
   */
  emptyDataMessage?: React.ReactNode;
  /**
   * Formatter for the independent values in the chart. For example, if the values are Unix
   * timestamps, this could be a function that formats them into a human-readable date string.
   *
   * @default String
   */
  formatIndependentValue?: (value: TIndependentValue) => string;
  /**
   * Formatter for the independent values in the chart, only used in the tooltip.
   * Can return a React node to display rich content like avatars.
   *
   * @default formatIndependentValue
   */
  formatIndependentValueTooltip?: (value: TIndependentValue) => React.ReactNode;
  /**
   * Formatter for the independent values in the tooltip that takes the active series into account.
   * If provided, this is used instead of formatIndependentValueTooltip.
   * Useful for showing different dates based on which series is being hovered.
   */
  formatIndependentValueTooltipForSeries?: (
    value: TIndependentValue,
    activeSeriesKey: ChartDataKeyType | null,
  ) => React.ReactNode;
  /**
   * Formatter for the dependent values in the chart. For example, if the values are in
   * milliseconds, this could be a function that formats them into a human-readable duration string.
   * To control how individual series are rendered in their tooltip, use the `formatValue` property
   * of the series config items.
   *
   * @default String
   */
  formatValue?: (value: TDependentValue) => string;
  /**
   * Formatter for the dependent values in the chart, only used in the tooltip. See {@link formatValue}.
   *
   * @default formatValue
   */
  formatValueTooltip?: (value: TDependentValue) => string;
  /**
   * Series that should be grouped together on the chart. For bar charts, grouped series will be
   * shown in a stacked bar. For line charts, there is no visual difference between grouped and
   * ungrouped series.
   */
  groups?: LoadState<readonly (readonly TDependentKey[])[]>;
  /**
   * The set of series hidden by legend clicks. If not provided, the chart will manage its own
   * state internally.
   */
  hiddenSeries?: ReadonlySet<TDependentKey>;
  /**
   * Hides the axes and ticks.
   *
   * @default false
   */
  hideAxes?: boolean;
  /**
   * Label for the independent (X) axis.
   */
  independentAxisLabel?: string;
  /**
   * The domain of the independent axis. If not provided, the domain will be automatically calculated.
   */
  independentDomain?: ChartAxisDomain<TIndependentValue>;
  /**
   * A custom renderer for the independent axis ticks.
   */
  independentTick?: (value: TIndependentValue) => React.ReactNode;
  /**
   * The type of the independent values.
   *
   * @default ChartAxisType.Category
   */
  independentType?: ChartAxisType;
  /**
   * Whether to show the legend for the chart.
   *
   * @default false
   */
  legend?: boolean;
  /**
   * Additional padding above the legend. Useful when overlaying content above the legend.
   */
  legendPaddingTop?: number;
  /**
   * Reference lines to draw on the chart.
   */
  referenceLines?: LoadState<readonly ChartReferenceLine<TIndependentValue, TDependentValue>[]>;
  /**
   * If provided, used to render additional data in the tooltip.
   */
  renderTooltipData?: (
    data: TTooltipData,
    independentValue: TIndependentValue | null,
  ) => React.ReactNode;
  /**
   * Style to apply to the chart.
   */
  style?: React.CSSProperties;
  /**
   * Whether to show the tooltip.
   *
   * @default true
   */
  tooltip?: boolean;

  /**
   * The series that the user is currently hovering over, if any. If not provided, the chart will
   * manage its own state internally. If provided, {@link setActiveSeries} must also be provided.
   */
  activeSeries?: TDependentKey;
  /**
   * If provided, highlights the bar(s) with this independent value by dimming all other bars.
   * Similar to hover behavior but for a specific independent value.
   */
  highlightedIndependentValue?: TIndependentValue;
  /**
   * If provided, this callback will be called when the user clicks on a data point in the chart
   * (either a bar or a line).
   */
  onClickDataPoint?: (value: TIndependentValue, seriesKey: TDependentKey) => void;
  /**
   * If provided, this callback will be called when the user clicks on an empty area of the chart
   * (not on a bar or line).
   */
  onClickEmpty?: () => void;
  /**
   * The setter for the active series. If not provided, the chart will manage its own state
   * internally. If provided, {@link activeSeries} must also be provided.
   */
  setActiveSeries?: React.Dispatch<React.SetStateAction<TDependentKey | undefined>>;
  /**
   * The setter for the hidden series. If not provided, the chart will manage its own state
   * internally. If provided, {@link hiddenSeries} must also be provided.
   */
  setHiddenSeries?: React.Dispatch<React.SetStateAction<ReadonlySet<TDependentKey>>>;
  /**
   * Optional function to sort the chart data. Called whenever the data or hidden series change,
   * which lets sort logic react to series visibility (e.g. excluding hidden series from totals).
   * Receives a mutable copy of the data array — the function may sort in place or return a new
   * array.
   */
  sortData?: (
    data: ChartData<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>,
    hiddenSeries: ReadonlySet<TDependentKey>,
  ) => ChartData<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue>;
}

export function Chart<
  TIndependentKey extends ChartDataKeyType,
  TDependentKey extends ChartDataKeyType,
  TIndependentValue extends ChartDataValueType,
  TDependentValue extends ChartDataValueType,
  TTooltipData = unknown,
>({
  activeSeries: activeSeriesExternal,
  allowDependentTickDecimals = false,
  config,
  containerRef,
  data,
  dependentAxisLabel,
  dependentDomain,
  dependentKeys,
  dependentType = ChartAxisType.Number,
  emptyData = [],
  emptyDataMessage,
  formatIndependentValue = String,
  formatIndependentValueTooltip,
  formatIndependentValueTooltipForSeries,
  formatValue = String,
  formatValueTooltip = formatValue,
  groups,
  hiddenSeries: hiddenSeriesExternal,
  hideAxes = false,
  highlightedIndependentValue,
  independentAxisLabel,
  independentDomain,
  independentKey,
  independentTick,
  independentType = ChartAxisType.Category,
  legend = false,
  legendPaddingTop = 0,
  onClickDataPoint,
  onClickEmpty,
  referenceLines,
  renderTooltipData,
  setActiveSeries: setActiveSeriesExternal,
  setHiddenSeries: setHiddenSeriesExternal,
  sortData,
  style,
  tooltip = true,
  ...props
}: ChartProps<TIndependentKey, TDependentKey, TIndependentValue, TDependentValue, TTooltipData>) {
  const [activeSeriesInternal, setActiveSeriesInternal] = useState<TDependentKey>();

  const activeSeries = activeSeriesExternal ?? activeSeriesInternal;
  const setActiveSeries = setActiveSeriesExternal ?? setActiveSeriesInternal;

  const { hiddenSeries, toggleSeriesVisibility } = useToggleSeriesVisibility({
    dependentKeys,
    hiddenSeriesExternal,
    setHiddenSeriesExternal,
  });

  // Apply the optional sort callback. Recomputing when hiddenSeries changes lets sort logic
  // react to users hiding series via the legend (e.g. so bars sorted by total stay sorted by the
  // total of only the visible series).
  const sortedData = useMemo(() => {
    if (sortData == null) return data;
    return LoadState.map(data, value => sortData([...value], hiddenSeries));
  }, [data, hiddenSeries, sortData]);

  const isEmpty = LoadState.isLoaded(data) && data.value.length === 0;
  const ChartContext = getChartContext<TDependentKey, TIndependentValue, TDependentValue>();
  const groupIndices = useMemo(() => {
    const result = new Map<TDependentKey, number>();
    if (groups == null) return result;
    if (!LoadState.isReady(groups)) return result;

    const count = groups.value.length;
    for (const [i, group] of groups.value.entries()) {
      for (const key of group) {
        // We need to make sure that stacks in different layers are not grouped together - Recharts
        // can't handle it.
        const layer = config?.[key]?.layer ?? 0;
        result.set(key, count * layer + i);
      }
    }

    return result;
  }, [config, groups]);

  /** Maps each dependent key to its resolved color using the full unfiltered key list,
   *  so that hiding a series doesn't shift other series' colors. */
  const colorByKey = useMemo(() => {
    const map = new Map<TDependentKey, string>();
    if (!LoadState.isReady(dependentKeys)) return map;
    dependentKeys.value.forEach((key, i) => {
      map.set(key, config?.[key]?.color ?? getDefaultSeriesColor(i));
    });
    return map;
  }, [config, dependentKeys]);

  /** Orders dependent keys by layer. */
  const orderedDependentKeys = useMemo(
    () =>
      LoadState.map(dependentKeys, dependentKeys => {
        const byLayer = groupBy(dependentKeys, key => config?.[key]?.layer ?? 0);
        const layers = [...byLayer.keys()].sort((a, b) => b - a);
        const result: TDependentKey[][] = [];
        for (const layer of layers) {
          result.push(byLayer.get(layer) ?? []);
        }

        return result;
      }),
    [config, dependentKeys],
  );

  // Recharts has an ugly animation if the chart scale changes at the same time the data loads. To
  // avoid that, we render the actual lines/bars/areas only after the data has loaded. This gives
  // the chart time to adjust its scale on one render, then animate the data in nicely on the next
  // render.
  //
  // We track whether animation has already happened via a ref. If the component remounts with
  // data already loaded (e.g., during polling), we skip the animation phase by initializing
  // renderData to true immediately.
  const hasAnimatedRef = useRef(LoadState.isReady(data));
  const [renderData, setRenderData] = useState(() => LoadState.isReady(data));

  // Track whether animation should be active. Animation is only enabled when data transitions
  // from loading → ready. If we mount with data already ready (e.g., cached data when switching
  // chart types), we skip animation entirely.
  const [isAnimationActive, setIsAnimationActive] = useState(false);

  useEffect(() => {
    const isReady = LoadState.isReady(data);

    /* eslint-disable react-hooks/set-state-in-effect -- coordinate render/animation state with async data loading transitions */
    if (isReady && !hasAnimatedRef.current) {
      // First time data becomes ready after loading - animate it in
      hasAnimatedRef.current = true;
      setRenderData(true);
      setIsAnimationActive(true);
    } else if (isReady) {
      // Data was already ready (e.g., remount with cached data, or polling update) - show without animation
      setRenderData(true);
      setIsAnimationActive(false);
    } else {
      // Data is loading - hide the chart elements and reset animation flag
      hasAnimatedRef.current = false;
      setRenderData(false);
      setIsAnimationActive(false);
    }
    /* eslint-enable react-hooks/set-state-in-effect */
  }, [data]);

  const hasCustomTick = independentTick != null;

  const maxTickLabelLength = useMemo(() => {
    const unwrappedData = LoadState.unwrap(data);
    const unwrappedKeys = LoadState.unwrap(dependentKeys);
    if (unwrappedData == null || unwrappedData.length === 0) return null;
    if (unwrappedKeys == null || unwrappedKeys.length === 0) return null;

    let maxAbsValue = 0;
    for (const point of unwrappedData) {
      let positiveSum = 0;
      let negativeSum = 0;
      for (const key of unwrappedKeys) {
        const value = (point as Record<string, unknown>)[key as string];
        if (typeof value === "number" && !isNaN(value)) {
          if (value >= 0) positiveSum += value;
          else negativeSum += Math.abs(value);
        }
      }
      maxAbsValue = Math.max(maxAbsValue, positiveSum, negativeSum);
    }

    if (maxAbsValue === 0) return null;

    const posFormatted = formatValue(maxAbsValue as TDependentValue);
    const negFormatted = formatValue(-maxAbsValue as TDependentValue);
    return Math.max(posFormatted.length, negFormatted.length);
  }, [data, dependentKeys, formatValue]);

  const dependentAxisWidth = useMemo(() => {
    if (hideAxes) return 0;
    if (maxTickLabelLength == null) return 40;

    const CHAR_WIDTH_PX = 7;
    const TICK_ROUNDING_BUFFER_CHARS = 1;
    const PADDING_PX = 12;
    return Math.max(
      40,
      (maxTickLabelLength + TICK_ROUNDING_BUFFER_CHARS) * CHAR_WIDTH_PX + PADDING_PX,
    );
  }, [hideAxes, maxTickLabelLength]);

  // Track whether a data point (bar/line) was just clicked in this event cycle.
  // Used to prevent the chart-level onClick from firing onClickEmpty incorrectly.
  const dataPointClickedRef = useRef(false);

  return (
    <ChartContext.Provider
      value={{ activeSeries, config, hiddenSeries, setActiveSeries, toggleSeriesVisibility }}
    >
      <ChartContainer ref={containerRef} {...props}>
        <Recharts.ResponsiveContainer debounce={200}>
          <Recharts.ComposedChart
            accessibilityLayer
            data={isEmpty ? emptyData : LoadState.unwrap(sortedData, emptyData)}
            style={style}
            stackOffset="sign"
            onClick={
              onClickEmpty != null ?
                () => {
                  // If a data point (bar/line) was clicked, the Bar/Line onClick already
                  // handled it, skip the empty-space check to avoid false positives.
                  if (dataPointClickedRef.current) {
                    dataPointClickedRef.current = false;
                    return;
                  }

                  onClickEmpty();
                }
              : undefined
            }
          >
            <defs>
              {/* These patterns allow us to make diagonally striped bars. */}
              <pattern
                id="diagonalLines"
                width="10"
                height="10"
                patternTransform="rotate(130 0 0) scale(0.8)"
                patternUnits="userSpaceOnUse"
              >
                <rect x="0" y="0" width="10" height="10" style={{ fill: "white" }} />
                <line
                  x1="0"
                  y1="0"
                  x2="0"
                  y2="10"
                  style={{ opacity: 0.6, stroke: "black", strokeWidth: 10 }}
                />
              </pattern>

              <mask id="diagonalLinesMask" x="0" y="0" width="1" height="1">
                <rect x="0" y="0" width="4096" height="4096" fill="url(#diagonalLines)" />
              </mask>
            </defs>

            <Recharts.XAxis
              xAxisId={0}
              dataKey={independentKey}
              type={independentType === ChartAxisType.Category ? "category" : "number"}
              scale={independentType === ChartAxisType.Time ? "time" : "auto"}
              domain={getRechartsDomain(independentDomain)}
              minTickGap={20}
              tick={
                hideAxes ? false
                : hasCustomTick ?
                  <ChartTick renderTick={independentTick} />
                : undefined
              }
              axisLine={hideAxes ? false : undefined}
              height={hideAxes ? 0 : undefined}
              interval={hasCustomTick ? 0 : undefined}
              tickMargin={X_AXIS_TICK_MARGIN}
              tickLine={false}
              tickFormatter={formatIndependentValue}
              padding={independentType === ChartAxisType.Time ? { left: 20, right: 20 } : "gap"}
              label={
                independentAxisLabel != null && !hideAxes ?
                  { offset: 5, position: "bottom", value: independentAxisLabel }
                : undefined
              }
            />

            {/* This X axis is not rendered, but it allows us to support the `layer` series configuration.
                Used for projected values that overlay on top of the main bars. */}
            <Recharts.XAxis
              xAxisId={1}
              dataKey={independentKey}
              type={independentType === ChartAxisType.Category ? "category" : "number"}
              scale={independentType === ChartAxisType.Time ? "time" : "auto"}
              domain={getRechartsDomain(independentDomain)}
              minTickGap={20}
              tick={false}
              axisLine={false}
              height={0}
              tickMargin={X_AXIS_TICK_MARGIN}
              tickLine={false}
              padding={independentType === ChartAxisType.Time ? { left: 20, right: 20 } : "gap"}
            />

            <Recharts.YAxis
              yAxisId={ChartDependentAxis.Left}
              orientation="left"
              tickLine={false}
              type={dependentType === ChartAxisType.Category ? "category" : "number"}
              scale={dependentType === ChartAxisType.Time ? "time" : "auto"}
              tickFormatter={formatValue}
              tick={hideAxes ? false : undefined}
              axisLine={hideAxes ? false : undefined}
              width={dependentAxisWidth}
              allowDecimals={allowDependentTickDecimals}
              domain={getRechartsDomain(dependentDomain)}
              label={
                dependentAxisLabel != null && !hideAxes ?
                  { angle: -90, offset: 10, position: "left", value: dependentAxisLabel }
                : undefined
              }
            />

            <Recharts.YAxis
              yAxisId={ChartDependentAxis.Right}
              orientation="right"
              tickLine={false}
              type={dependentType === ChartAxisType.Category ? "category" : "number"}
              scale={dependentType === ChartAxisType.Time ? "time" : "auto"}
              tickFormatter={formatValue}
              tick={hideAxes ? false : undefined}
              axisLine={hideAxes ? false : undefined}
              width={dependentAxisWidth}
              allowDecimals={allowDependentTickDecimals}
              domain={getRechartsDomain(dependentDomain)}
            />

            <Recharts.CartesianGrid vertical={false} />

            {renderData &&
              LoadState.unwrap(orderedDependentKeys)?.flatMap(keys =>
                keys.map((key, i) => {
                  const keyConfig = config?.[key];

                  const type = keyConfig?.type ?? ChartSeriesType.Bar;

                  const color = colorByKey.get(key) ?? keyConfig?.color ?? getDefaultSeriesColor(i);
                  const opacity = keyConfig?.opacity ?? 1;
                  const inactiveOpacity = keyConfig?.inactiveOpacity ?? 0.3 * opacity;
                  const dashed = keyConfig?.dashed ?? false;
                  const dot = keyConfig?.dot ?? true;
                  const legend = keyConfig?.legend ?? true;
                  const seriesInteractive = keyConfig?.interactive ?? true;
                  const layer = keyConfig?.layer ?? 0;
                  const dependentAxis = keyConfig?.dependentAxis ?? ChartDependentAxis.Left;
                  const interactive = onClickDataPoint != null && seriesInteractive;
                  const connectNulls = keyConfig?.connectNulls ?? false;

                  const isHidden = hiddenSeries.has(key);
                  const otherSeriesActive = activeSeries != null && activeSeries !== key;

                  const setSeriesActive = (active: boolean) => {
                    if (active) {
                      setActiveSeries(key);
                    } else {
                      setActiveSeries(curr => (curr === key ? undefined : curr));
                    }
                  };

                  switch (type) {
                    case ChartSeriesType.Area:
                      return (
                        <Recharts.Area
                          key={key}
                          dataKey={key}
                          hide={isHidden}
                          animationDuration={ANIMATION_DURATION}
                          isAnimationActive={isAnimationActive}
                          xAxisId={layer}
                          yAxisId={dependentAxis}
                          opacity={otherSeriesActive ? inactiveOpacity : opacity}
                          legendType={legend ? undefined : "none"}
                          fill={color}
                          stroke={color}
                          mask={dashed ? "url(#diagonalLinesMask)" : undefined}
                          stackId={groupIndices.get(key)}
                          connectNulls={connectNulls}
                          strokeWidth={STROKE_WIDTH}
                          dot={dot ? { mask: "", r: 1.2 } : false}
                          activeDot={false}
                        />
                      );

                    case ChartSeriesType.Bar: {
                      // Calculate per-bar opacity when an independent value is highlighted
                      const chartData = LoadState.unwrap(sortedData, emptyData);
                      const hasHighlight = highlightedIndependentValue != null;

                      return (
                        <Recharts.Bar
                          key={key}
                          dataKey={key}
                          hide={isHidden}
                          animationDuration={ANIMATION_DURATION}
                          isAnimationActive={isAnimationActive}
                          xAxisId={layer}
                          yAxisId={dependentAxis}
                          opacity={
                            hasHighlight ? undefined
                            : otherSeriesActive ?
                              inactiveOpacity
                            : opacity
                          }
                          legendType={legend ? undefined : "none"}
                          fill={color}
                          mask={dashed ? "url(#diagonalLinesMask)" : undefined}
                          stackId={groupIndices.get(key)}
                          maxBarSize={100} // without this, bars aren't rendered when there's only 1 data point
                          shape={MinHeightBar} // ensure small non-zero values remain visible and clickable
                          onFocus={() => setSeriesActive(seriesInteractive && true)}
                          onMouseEnter={() => setSeriesActive(seriesInteractive && true)}
                          onBlur={() => setSeriesActive(false)}
                          onMouseLeave={() => setSeriesActive(false)}
                          onClick={(barData: unknown) => {
                            if (!interactive) return;

                            if (barData == null || typeof barData !== "object") return;
                            const independentValue = (
                              barData as Record<TIndependentKey, TIndependentValue | undefined>
                            )[independentKey];
                            if (independentValue == null) return;

                            dataPointClickedRef.current = true;
                            onClickDataPoint(independentValue, key);
                          }}
                          style={interactive ? { cursor: "pointer" } : {}}
                        >
                          {hasHighlight &&
                            chartData.map((entry, index) => {
                              const entryIndependentValue = entry[independentKey];
                              const isHighlighted =
                                entryIndependentValue === highlightedIndependentValue;
                              const cellOpacity = isHighlighted ? opacity : inactiveOpacity;
                              return (
                                <Recharts.Cell
                                  key={`cell-${index}`}
                                  opacity={otherSeriesActive ? inactiveOpacity : cellOpacity}
                                />
                              );
                            })}
                        </Recharts.Bar>
                      );
                    }

                    case ChartSeriesType.Line:
                      return (
                        <React.Fragment key={key}>
                          <Recharts.Line
                            xAxisId={layer}
                            yAxisId={dependentAxis}
                            key={key}
                            dataKey={key}
                            hide={isHidden}
                            animationDuration={ANIMATION_DURATION}
                            isAnimationActive={isAnimationActive}
                            opacity={otherSeriesActive ? inactiveOpacity : opacity}
                            connectNulls={connectNulls}
                            type="monotoneX"
                            legendType={legend ? undefined : "none"}
                            stroke={color}
                            dot={dot}
                            fill={color}
                            activeDot={false}
                            strokeWidth={STROKE_WIDTH}
                            strokeDasharray={dashed ? "10 10" : undefined}
                          />

                          {/* This line allows the user to hover over a wider target to set a line as active. */}
                          {seriesInteractive && !isHidden && (
                            <Recharts.Line
                              xAxisId={layer}
                              yAxisId={dependentAxis}
                              key={`${key}-mouse-target`}
                              dataKey={key}
                              data-series-key={key}
                              animationDuration={0}
                              opacity={0}
                              connectNulls={connectNulls}
                              type="monotoneX"
                              legendType="none"
                              tooltipType="none"
                              dot={
                                interactive ?
                                  {
                                    onClick: a => {
                                      if (!interactive) return;

                                      const data = (a as Record<string, unknown>).payload;

                                      if (data == null || typeof data !== "object") return;
                                      const independentValue = (
                                        data as Record<
                                          TIndependentKey,
                                          TIndependentValue | undefined
                                        >
                                      )[independentKey];
                                      if (independentValue == null) return;

                                      dataPointClickedRef.current = true;
                                      onClickDataPoint(independentValue, key);
                                    },
                                    onMouseEnter: () => setSeriesActive(true),
                                    onMouseLeave: () => setSeriesActive(false),
                                    opacity: 0,
                                    r: 10,
                                    style: { cursor: "pointer" },
                                  }
                                : false
                              }
                              activeDot={false}
                              strokeWidth={20}
                              onFocus={() => setSeriesActive(interactive && true)}
                              onMouseEnter={() => setSeriesActive(interactive && true)}
                              onBlur={() => setSeriesActive(false)}
                              onMouseLeave={() => setSeriesActive(false)}
                            />
                          )}
                        </React.Fragment>
                      );

                    default:
                      unreachable(type);
                  }
                }),
              )}

            {referenceLines != null &&
              LoadState.unwrap(referenceLines)?.map(line => {
                const getValue = (val: ChartDataValueType): number | string =>
                  Array.isArray(val) ? val[0] : val;

                return (
                  <Recharts.ReferenceLine
                    key={line.label ?? `${line.type}-${String(line.value)}`}
                    xAxisId={0}
                    yAxisId={ChartDependentAxis.Left}
                    label={<ChartReferenceLineLabel label={line.label} side={line.side} />}
                    x={line.type === "independent" ? getValue(line.value) : undefined}
                    y={line.type === "dependent" ? getValue(line.value) : undefined}
                    strokeWidth={1.5}
                    stroke={line.color ?? "hsl(var(--muted-foreground)/0.3)"}
                    ifOverflow="visible"
                    strokeDasharray={line.dashed === true ? "10 10" : undefined}
                  />
                );
              })}

            {tooltip && (
              <Recharts.Tooltip
                content={
                  <ChartTooltip
                    formatValue={formatValueTooltip}
                    formatIndependentValue={formatIndependentValueTooltip ?? formatIndependentValue}
                    formatIndependentValueForSeries={formatIndependentValueTooltipForSeries}
                    renderTooltipData={renderTooltipData}
                  />
                }
              />
            )}

            {legend && (
              <Recharts.Legend
                itemSorter={null} // sorts legend items by the order they appear in the data, instead of alphabetically (default behavior)
                wrapperStyle={{ paddingTop: (hasCustomTick ? 16 : 0) + legendPaddingTop }}
                content={props => <ChartLegend payload={props.payload} />}
              />
            )}
          </Recharts.ComposedChart>
        </Recharts.ResponsiveContainer>

        {isEmpty && <ChartMessageCard.Empty emptyMessage={emptyDataMessage} />}
        {LoadState.isReloading(data) && <ChartMessageCard.Loading />}
        {LoadState.isError(data) && <ChartMessageCard.Error error={data.error} />}
      </ChartContainer>
    </ChartContext.Provider>
  );
}
