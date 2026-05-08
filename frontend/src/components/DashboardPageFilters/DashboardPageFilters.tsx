"use client";

import { Command } from "@/components/molecules/Command";
import { Popover } from "@/components/molecules/Popover";
import { cn } from "@/lib/cn";
import { CalendarRange, ChevronDown, Clock } from "lucide-react";
import React, { useState } from "react";

import { FilterPill } from "./FilterPill";
import {
  DATE_RANGES,
  Granularity,
  UseDashboardFiltersResult,
} from "./useDashboardFilters";

const GRANULARITY_LABELS: Record<Granularity, string> = {
  hour: "hour",
  day: "day",
};

export interface DashboardPageFiltersProps {
  className?: string;
  result: UseDashboardFiltersResult;
  right?: React.ReactNode;
}

/**
 * Sticky filter bar with date-range and granularity Popover-driven
 * pills. Visual rhythm matches WorkWeave's DashboardPageFilters.
 */
export function DashboardPageFilters({
  className,
  result,
  right,
}: DashboardPageFiltersProps) {
  return (
    <div
      className={cn(
        "sticky top-0 z-10 flex w-full max-w-content-width items-center gap-2 bg-muted px-6 py-3",
        className,
      )}
    >
      <DateRangePill result={result} />
      <GranularityPill result={result} />
      <div className="flex-1" />
      {right}
    </div>
  );
}

function DateRangePill({ result }: { result: UseDashboardFiltersResult }) {
  const [open, setOpen] = useState(false);
  return (
    <FilterPill>
      <CalendarRange className="size-3.5" />
      <span className="font-medium">Date</span>
      <span className="text-muted-foreground">is</span>
      <Popover open={open} onOpenChange={setOpen}>
        <Popover.Trigger>
          <FilterPill.Button className="-mr-2 pr-2">
            {result.filters.range.label.toLowerCase()}
            <ChevronDown className="size-3.5" />
          </FilterPill.Button>
        </Popover.Trigger>
        <Popover.Content className="w-64 p-1" align="start">
          <Command>
            <Command.List>
              {DATE_RANGES.map(opt => (
                <Command.Item
                  key={opt.id}
                  onSelect={() => {
                    result.setRangeId(opt.id);
                    setOpen(false);
                  }}
                >
                  {opt.label}
                </Command.Item>
              ))}
            </Command.List>
          </Command>
        </Popover.Content>
      </Popover>
    </FilterPill>
  );
}

function GranularityPill({ result }: { result: UseDashboardFiltersResult }) {
  const [open, setOpen] = useState(false);
  const options: Granularity[] = ["hour", "day"];
  return (
    <FilterPill>
      <Clock className="size-3.5" />
      <span className="font-medium">Granularity</span>
      <span className="text-muted-foreground">is</span>
      <Popover open={open} onOpenChange={setOpen}>
        <Popover.Trigger>
          <FilterPill.Button className="-mr-2 pr-2">
            {GRANULARITY_LABELS[result.filters.granularity]}
            <ChevronDown className="size-3.5" />
          </FilterPill.Button>
        </Popover.Trigger>
        <Popover.Content className="w-48 p-1" align="start">
          <Command>
            <Command.List>
              {options.map(g => (
                <Command.Item
                  key={g}
                  onSelect={() => {
                    result.setGranularity(g);
                    setOpen(false);
                  }}
                >
                  {GRANULARITY_LABELS[g]}
                </Command.Item>
              ))}
            </Command.List>
          </Command>
        </Popover.Content>
      </Popover>
    </FilterPill>
  );
}
