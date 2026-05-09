import * as Recharts from "recharts";

import { ChartAxisDomain } from "../types";

/**
 * Converts our stricter domain type to Recharts' domain type.
 */
export function getRechartsDomain<TValue>(
  domain?: ChartAxisDomain<TValue>,
): Recharts.XAxisProps["domain"] {
  if (domain == null) return undefined;

  if (typeof domain === "function") {
    return (bounds, allowDataOverflow) =>
      domain(bounds as [TValue, TValue], allowDataOverflow) as [number, number];
  }

  return domain as [number, number];
}
