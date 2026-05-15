import { LoadState } from "@/tools/LoadState";

import { useCallback, useMemo, useState } from "react";

import { ChartDataKeyType } from "./types";

interface UseToggleSeriesVisibilityOptions<TDependentKey extends ChartDataKeyType> {
  dependentKeys: LoadState<readonly TDependentKey[]>;
  hiddenSeriesExternal?: ReadonlySet<TDependentKey>;
  setHiddenSeriesExternal?: React.Dispatch<React.SetStateAction<ReadonlySet<TDependentKey>>>;
}

interface UseToggleSeriesVisibilityResult<TDependentKey extends ChartDataKeyType> {
  hiddenSeries: ReadonlySet<TDependentKey>;
  toggleSeriesVisibility: (key: TDependentKey, exclusive?: boolean) => void;
}

export function useToggleSeriesVisibility<TDependentKey extends ChartDataKeyType>({
  dependentKeys,
  hiddenSeriesExternal,
  setHiddenSeriesExternal,
}: UseToggleSeriesVisibilityOptions<TDependentKey>): UseToggleSeriesVisibilityResult<TDependentKey> {
  if ((hiddenSeriesExternal == null) !== (setHiddenSeriesExternal == null)) {
    throw new Error("hiddenSeries and setHiddenSeries must be provided together");
  }

  const [hiddenSeriesInternal, setHiddenSeriesInternal] = useState<ReadonlySet<TDependentKey>>(
    new Set(),
  );
  const isControlled = hiddenSeriesExternal != null && setHiddenSeriesExternal != null;
  const hiddenSeries = isControlled ? hiddenSeriesExternal : hiddenSeriesInternal;
  const setHiddenSeries = isControlled ? setHiddenSeriesExternal : setHiddenSeriesInternal;

  const allKeys = useMemo(
    () => (LoadState.isReady(dependentKeys) ? [...dependentKeys.value] : []),
    [dependentKeys],
  );

  const toggleSeriesVisibility = useCallback(
    (key: TDependentKey, exclusive?: boolean) => {
      if (exclusive === true) {
        setHiddenSeries(prev => {
          const visibleKeys = allKeys.filter(k => !prev.has(k));
          // If the clicked key is already the only visible one, restore all
          if (visibleKeys.length === 1 && visibleKeys[0] === key) {
            return new Set();
          }
          // Otherwise, hide everything except the clicked key
          return new Set(allKeys.filter(k => k !== key));
        });
        return;
      }

      setHiddenSeries(prev => {
        const next = new Set(prev);
        if (next.has(key)) {
          next.delete(key);
        } else {
          next.add(key);
        }
        return next;
      });
    },
    [allKeys, setHiddenSeries],
  );

  return { hiddenSeries, toggleSeriesVisibility };
}
