"use client";

import { Kbd } from "@/components/atoms/Kbd";
import { Command } from "@/components/molecules/Command";
import { Popover } from "@/components/molecules/Popover";
import { Tooltip } from "@/components/molecules/Tooltip";
import { cn } from "@/lib/cn";
import { Calendar, Check, Clock } from "lucide-react";
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
  week: "week",
};

export interface DashboardPageFiltersProps {
  className?: string;
  result: UseDashboardFiltersResult;
}

/**
 * Dashboard filter bar. Mirrors WorkWeave's DashboardPageFilters: each
 * pill is wrapped in a Tooltip with a keyboard hint, opens a Popover
 * with a Command list, and the bar itself is a flat row that wraps on
 * narrow screens. Rendered inline with the page content (no separator).
 */
export function DashboardPageFilters({ className, result }: DashboardPageFiltersProps) {
  return (
    <div className={cn("flex flex-row flex-wrap items-start gap-4 gap-y-2", className)}>
      <GranularityPill result={result} />
      <DateRangePill result={result} />
    </div>
  );
}

function DateRangePill({ result }: { result: UseDashboardFiltersResult }) {
  const [open, setOpen] = useState(false);
  const currentId = result.filters.range.id;
  return (
    <Tooltip
      content={
        <span className="flex items-center gap-1.5">
          Change date{" "}
          <Kbd inverted size="sm">
            D
          </Kbd>
        </span>
      }
      interactiveChild
    >
      <span className="inline-flex">
        <FilterPill>
          <Calendar className="size-3.5" />
          <span className="font-medium">Date</span>
          <span className="text-muted-foreground">is</span>
          <Popover open={open} onOpenChange={setOpen}>
            <Popover.Trigger>
              <FilterPill.Button className="-mr-2 pr-2">
                {result.filters.range.label.toLowerCase()}
              </FilterPill.Button>
            </Popover.Trigger>
            <Popover.Content className="w-64 p-1" align="start">
              <Command>
                <Command.Input placeholder="Search date range" />
                <Command.List>
                  {DATE_RANGES.map(opt => (
                    <Command.Item
                      key={opt.id}
                      value={opt.label}
                      onSelect={() => {
                        result.setRangeId(opt.id);
                        setOpen(false);
                      }}
                    >
                      <div className="flex items-center gap-2">
                        <div className="flex size-4 items-center justify-center">
                          {currentId === opt.id && <Check className="size-3.5" />}
                        </div>
                        {opt.label}
                      </div>
                    </Command.Item>
                  ))}
                </Command.List>
              </Command>
            </Popover.Content>
          </Popover>
        </FilterPill>
      </span>
    </Tooltip>
  );
}

function GranularityPill({ result }: { result: UseDashboardFiltersResult }) {
  const [open, setOpen] = useState(false);
  const options: Granularity[] = ["hour", "day", "week"];
  const current = result.filters.granularity;
  return (
    <Tooltip
      content={
        <span className="flex items-center gap-1.5">
          Change granularity{" "}
          <Kbd inverted size="sm">
            G
          </Kbd>
        </span>
      }
      interactiveChild
    >
      <span className="inline-flex">
        <FilterPill>
          <Clock className="size-3.5" />
          <span className="font-medium">Granularity</span>
          <span className="text-muted-foreground">is</span>
          <Popover open={open} onOpenChange={setOpen}>
            <Popover.Trigger>
              <FilterPill.Button className="-mr-2 pr-2">
                {GRANULARITY_LABELS[current]}
              </FilterPill.Button>
            </Popover.Trigger>
            <Popover.Content className="w-64 p-1" align="start">
              <Command>
                <Command.Input placeholder="Search granularity" />
                <Command.List>
                  {options.map(g => (
                    <Command.Item
                      key={g}
                      value={GRANULARITY_LABELS[g]}
                      onSelect={() => {
                        result.setGranularity(g);
                        setOpen(false);
                      }}
                    >
                      <div className="flex items-center gap-2">
                        <div className="flex size-4 items-center justify-center">
                          {current === g && <Check className="size-3.5" />}
                        </div>
                        {GRANULARITY_LABELS[g]}
                      </div>
                    </Command.Item>
                  ))}
                </Command.List>
              </Command>
            </Popover.Content>
          </Popover>
        </FilterPill>
      </span>
    </Tooltip>
  );
}
