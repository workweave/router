"use client";

import { Button, ButtonProps } from "@/components/molecules/Button";
import { Appearance } from "@/components/types";
import { cn } from "@/lib/cn";
import { X } from "lucide-react";
import React, { forwardRef } from "react";

export interface FilterPillProps extends React.PropsWithChildren {
  onRemove?: () => void;
}

type FilterPillType = React.FC<FilterPillProps> & {
  Button: React.ForwardRefExoticComponent<
    Omit<ButtonProps, "appearance" | "size"> & React.RefAttributes<HTMLButtonElement>
  >;
};

/**
 * Active-filter pill: muted background, slot for an icon + label + a
 * Popover-trigger Button (via FilterPill.Button) that opens the picker.
 * Mirrors WorkWeave's filter bar pill aesthetic.
 */
const FilterPillButton = forwardRef<
  HTMLButtonElement,
  Omit<ButtonProps, "appearance" | "size">
>(function FilterPillButton({ className, ...props }, ref) {
  return (
    <Button
      {...props}
      ref={ref}
      appearance={Appearance.Hollow}
      className={cn("-m-1 -my-2 h-8 rounded-lg p-1", className)}
    />
  );
});
FilterPillButton.displayName = "FilterPill.Button";

export const FilterPill: FilterPillType = Object.assign(
  function FilterPill({ children, onRemove }: FilterPillProps) {
    return (
      <div className="flex shrink-0 grow-0 items-center gap-1.5 rounded-lg bg-muted px-2 py-1 text-sm">
        {children}
        {onRemove != null && (
          <FilterPillButton onClick={onRemove} className="-mr-2">
            <X className="size-3.5" />
          </FilterPillButton>
        )}
      </div>
    );
  },
  { Button: FilterPillButton },
);
