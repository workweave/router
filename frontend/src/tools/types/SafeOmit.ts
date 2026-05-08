/**
 * Construct a type with the properties of T except for those in type K. Same as the built-in
 * `Omit`, but with better type safety for the keys.
 */
export type SafeOmit<T, K extends keyof T> = Pick<T, Exclude<keyof T, K>>;
