import { cn } from "@/lib/cn";
import React from "react";

export interface SkeletonProps extends React.ComponentProps<"div"> {
  as?: "div" | "span";
  darker?: boolean;
}

export function Skeleton({
  as: Element = "div",
  className,
  darker = false,
  ...props
}: SkeletonProps) {
  return (
    <Element
      className={cn(
        "relative overflow-hidden rounded-md bg-muted after:pointer-events-none after:absolute after:inset-0 after:-translate-x-full after:animate-slide-infinite after:bg-gradient-to-r after:from-background/0 after:via-background/50 after:via-60% after:to-background/0",
        darker && "bg-muted-foreground/20",
        className,
      )}
      {...props}
    />
  );
}
