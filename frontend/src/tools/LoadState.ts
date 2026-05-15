function assert(cond: unknown, msg: string): asserts cond { if (!cond) throw new Error(msg); }

import { unreachable } from "./types";

const NOT_STARTED_LOADING = Symbol("NOT_STARTED_LOADING");
const LOADING = Symbol("LOADING");
const ERROR = Symbol("ERROR");
const LOADED = Symbol("LOADED");
const RELOADING = Symbol("RELOADING");

interface LoadStateNotStartedLoading {
  type: typeof NOT_STARTED_LOADING;
}

interface LoadStateLoading {
  type: typeof LOADING;
}

interface LoadStateError<T> {
  error: Error;
  /**
   * If defined, this is the value the load state had before it errored.
   */
  previousValue?: T;
  type: typeof ERROR;
}

interface LoadStateLoaded<T> {
  type: typeof LOADED;
  value: T;
}

interface LoadStateReloading<T> {
  type: typeof RELOADING;
  value: T;
}

export type LoadState<T> =
  | LoadStateError<T>
  | LoadStateLoaded<T>
  | LoadStateLoading
  | LoadStateNotStartedLoading
  | LoadStateReloading<T>;

const notStartedLoading: LoadStateNotStartedLoading = { type: NOT_STARTED_LOADING };
const loading: LoadStateLoading = { type: LOADING };

/**
 * Represents a value that is potentially not loaded yet, e.g. the result of a network request.
 */
export const LoadState = {
  error: <T>(error: Error, previousValue?: T): LoadStateError<T> => ({
    error,
    previousValue,
    type: ERROR,
  }),
  loaded: <T>(value: T): LoadStateLoaded<T> => ({ type: LOADED, value }),
  loading: () => loading,
  notStartedLoading: () => notStartedLoading,
  reloading: <T>(value: T): LoadStateReloading<T> => ({ type: RELOADING, value }),

  /**
   * Checks if the given `state` is a {@link LoadState}.
   */
  is: (state: unknown): state is LoadState<unknown> => {
    if (typeof state !== "object" || state == null) return false;
    if (!("type" in state)) return false;

    const type = state["type"];
    if (type === NOT_STARTED_LOADING) return true;
    if (type === LOADING) return true;
    if (type === ERROR) return true;
    if (type === LOADED) return true;
    if (type === RELOADING) return true;

    return false;
  },

  isError: <T>(state: LoadState<T>): state is LoadStateError<T> => state.type === ERROR,
  isLoaded: <T>(state: LoadState<T>): state is LoadStateLoaded<T> => state.type === LOADED,
  isLoading: <T>(state: LoadState<T>): state is LoadStateLoading => state.type === LOADING,
  isNotStartedLoading: <T>(state: LoadState<T>): state is LoadStateNotStartedLoading =>
    state.type === NOT_STARTED_LOADING,
  isReloading: <T>(state: LoadState<T>): state is LoadStateReloading<T> => state.type === RELOADING,

  /**
   * Checks if the given `LoadState` is ready (i.e. loaded or reloading).
   */
  isReady: <T>(state: LoadState<T>): state is LoadStateLoaded<T> | LoadStateReloading<T> =>
    state.type === LOADED || state.type === RELOADING,

  /**
   * Converts a load state of a load state to a single level of load state.
   */
  flatten: <T>(state: LoadState<LoadState<T>>): LoadState<T> => {
    if (LoadState.isLoaded(state)) return state.value;
    if (LoadState.isReloading(state) && LoadState.isError(state.value)) return state.value;
    if (LoadState.isReloading(state)) return LoadState.mapMaybeReloading(state.value);
    if (LoadState.isError(state)) {
      return LoadState.error(
        state.error,
        state.previousValue != null ? LoadState.unwrap(state.previousValue) : undefined,
      );
    }

    return state;
  },

  /**
   * Returns a new `LoadState` that is the same as the given `state`, but either loading or
   * reloading.
   */
  mapMaybeReloading: <T>(state: LoadState<T>): LoadState<T> => {
    if (LoadState.isReady(state)) return LoadState.reloading(state.value);
    return LoadState.loading();
  },

  /**
   * Returns the value if the state is ready, or the `previousValue` if the state is an error
   * with previous data. Returns `undefined` otherwise.
   */
  availableValue: <T>(state: LoadState<T>): T | undefined => {
    if (LoadState.isReady(state)) return state.value;
    if (LoadState.isError(state)) return state.previousValue;
    return undefined;
  },

  map,

  unwrap,
};

/**
 * Converts the first `n - 1` arguments into a single `LoadState`, using the last argument to
 * combine them all if every single one is ready.
 */
function map<T1, T2>(s1: LoadState<T1>, fn: (v1: T1) => T2): LoadState<T2>;
function map<T1, T2, T3>(
  s1: LoadState<T1>,
  s2: LoadState<T2>,
  fn: (v1: T1, v2: T2) => T3,
): LoadState<T3>;
function map<T1, T2, T3, T4>(
  s1: LoadState<T1>,
  s2: LoadState<T2>,
  s3: LoadState<T3>,
  fn: (v1: T1, v2: T2, v3: T3) => T4,
): LoadState<T4>;
function map<T1, T2, T3, T4, T5>(
  s1: LoadState<T1>,
  s2: LoadState<T2>,
  s3: LoadState<T3>,
  s4: LoadState<T4>,
  fn: (v1: T1, v2: T2, v3: T3, v4: T4) => T5,
): LoadState<T5>;
function map<T1, T2, T3, T4, T5, T6>(
  s1: LoadState<T1>,
  s2: LoadState<T2>,
  s3: LoadState<T3>,
  s4: LoadState<T4>,
  s5: LoadState<T5>,
  fn: (v1: T1, v2: T2, v3: T3, v4: T4, v5: T5) => T6,
): LoadState<T6>;
function map<T1, T2, T3, T4, T5, T6, T7>(
  s1: LoadState<T1>,
  s2: LoadState<T2>,
  s3: LoadState<T3>,
  s4: LoadState<T4>,
  s5: LoadState<T5>,
  s6: LoadState<T6>,
  fn: (v1: T1, v2: T2, v3: T3, v4: T4, v5: T5, v6: T6) => T7,
): LoadState<T7>;
function map(...args: unknown[]): LoadState<unknown> {
  assert(args.length >= 2, "expected at least 2 arguments");

  const mapper = args.pop();
  assert(typeof mapper === "function", "expected the last argument to be a function");

  const result = args.reduce((acc: LoadState<unknown[]>, state: unknown): LoadState<unknown[]> => {
    assert(LoadState.is(state), "expected all arguments to be LoadState");

    // Priority order:
    //  1. Error
    //  2. Loading
    //  3. Not started loading
    //  4. Reloading
    //  5. Loaded

    if (LoadState.isError(acc) && acc.previousValue !== undefined) {
      // push the current value to the error accumulator, if possible
      const value =
        LoadState.isError(state) && state.previousValue !== undefined ? state.previousValue
        : LoadState.isReady(state) ? state.value
        : undefined;

      if (value !== undefined) {
        acc.previousValue.push(value);
        return LoadState.error(acc.error, acc.previousValue);
      }

      return { ...acc, previousValue: undefined };
    }

    if (LoadState.isError(acc)) return acc;

    if (LoadState.isError(state) && state.previousValue !== undefined) {
      const value = LoadState.isReady(acc) ? acc.value : undefined;
      if (value !== undefined) {
        value.push(state.previousValue);
        return LoadState.error(state.error, value);
      }
      return { ...state, previousValue: undefined };
    }
    if (LoadState.isError(state)) return state as LoadState<unknown[]>;

    if (LoadState.isLoading(acc) || LoadState.isLoading(state)) return LoadState.loading();
    if (LoadState.isNotStartedLoading(acc) || LoadState.isNotStartedLoading(state)) {
      return LoadState.notStartedLoading();
    }

    const isReloading = LoadState.isReloading(acc) || LoadState.isReloading(state);
    acc.value.push(state.value);

    return isReloading ? LoadState.reloading(acc.value) : LoadState.loaded(acc.value);
  }, LoadState.loaded([]));

  if (LoadState.isError(result)) {
    return LoadState.error(
      result.error,
      result.previousValue != null ?
        (mapper as (...args: unknown[]) => unknown)(...result.previousValue)
      : undefined,
    );
  }

  if (LoadState.isNotStartedLoading(result) || LoadState.isLoading(result)) return result;
  if (LoadState.isReloading(result))
    return LoadState.reloading((mapper as (...args: unknown[]) => unknown)(...result.value));
  if (LoadState.isLoaded(result))
    return LoadState.loaded((mapper as (...args: unknown[]) => unknown)(...result.value));

  unreachable(result);
}

/**
 * Returns the value of the load state if it is ready, or `fallback` if it is not.`If `fallback` is
 * not provided, defaults to `undefined`.
 */
function unwrap<T>(state: LoadState<T>, fallback: T): T;
function unwrap<T>(state: LoadState<T>, fallback?: T): T | undefined;
function unwrap<T>(state: LoadState<T>, fallback?: T): T | undefined {
  if (LoadState.isReady(state)) return state.value;
  return fallback;
}
