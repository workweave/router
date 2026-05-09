import { cn } from "@/lib/cn";
import React from "react";

export interface ResponsiveGridProps extends React.HTMLAttributes<HTMLDivElement> {}

/**
 * Six-column responsive grid using container queries (@xl, @4xl). Pair
 * with `ResponsiveGrid.Full / .Large / .Medium / .Small` on children to
 * size them across breakpoints.
 */
export function ResponsiveGrid({ children, className, ...props }: ResponsiveGridProps) {
  return (
    <div className={cn("grid grid-cols-6 gap-4 @container", className)} {...props}>
      {children}
    </div>
  );
}

ResponsiveGrid.Small = cn("col-span-6 @xl:col-span-3 @4xl:col-span-2");
ResponsiveGrid.Medium = cn("col-span-6 @xl:col-span-3");
ResponsiveGrid.Large = cn("col-span-6 @xl:col-span-3 @4xl:col-span-4");
ResponsiveGrid.Full = cn("col-span-6");
