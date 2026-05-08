/**
 * Unreachable asserts that the given value is never reached. This is useful to ensure we handle new
 * enum/union values by showing a type error if we forget to handle them.
 *
 * @example
 * switch (type) {
 *   case "A":
 *     return "A";
 *   case "B":
 *     return "B";
 *   default:
 *     unreachable(type);
 * }
 */
export function unreachable(x: never): never {
  throw new Error(`Should be unreachable, but got: ${String(x)}`);
}
