/**
 * Returns a function that will only be called once. Subsequent calls will always return the same
 * result as the first call, regardless of the arguments passed.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function once<TArgs extends any[], TResult>(
  fn: (...args: TArgs) => TResult,
): (...args: TArgs) => TResult {
  let called = false;
  let result: TResult;

  return (...args: TArgs) => {
    if (called) return result;

    result = fn(...args);
    called = true;
    return result;
  };
}
