/**
 * Groups `values` by the result of `keySelector`.
 */
export function groupBy<TKey, TValue>(
  values: Iterable<TValue>,
  keySelector: (value: TValue) => TKey,
): Map<TKey, TValue[]> {
  const result = new Map<TKey, TValue[]>();
  for (const value of values) {
    const key = keySelector(value);
    const existing = result.get(key);
    if (existing != null) {
      existing.push(value);
    } else {
      result.set(key, [value]);
    }
  }
  return result;
}
