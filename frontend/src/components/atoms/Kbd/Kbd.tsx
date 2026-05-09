"use client";

import { cn } from "@/lib/cn";

import React from "react";

export interface KbdProps extends React.HTMLAttributes<HTMLElement> {
  /**
   * Inverted color scheme — use inside dark surfaces (e.g. tooltips).
   * @default false
   */
  inverted?: boolean;

  /**
   * Size of the keyboard shortcut element.
   * - default: standard size
   * - sm: smaller, for compact contexts like tooltips
   */
  size?: "default" | "sm";
}

export const Kbd = React.forwardRef<HTMLSpanElement, KbdProps>(
  ({ className, inverted = false, size = "default", ...props }, ref) => (
    <span
      ref={ref}
      className={cn(
        "grid place-items-center rounded border font-sans font-normal leading-none",
        size === "default" && "size-6 text-xs",
        size === "sm" && "size-5 text-[10px]",
        inverted ? "border-current text-current" : "border-border text-muted-foreground",
        className,
      )}
      {...props}
    />
  ),
);
Kbd.displayName = "Kbd";
