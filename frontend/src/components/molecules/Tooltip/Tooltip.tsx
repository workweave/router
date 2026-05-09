import { cn } from "@/lib/cn";
import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import React from "react";

export const TooltipProvider = TooltipPrimitive.Provider;

export interface TooltipProps
  extends React.ComponentProps<typeof TooltipPrimitive.Root>,
    Pick<React.ComponentProps<typeof TooltipPrimitive.Content>, "align" | "side"> {
  content: React.ReactNode;
  contentClassName?: string;
  arrow?: boolean;
  arrowClassName?: string;
  interactiveChild?: boolean;
  onClick?: (event: React.MouseEvent) => void;
  onKeyDown?: (event: React.KeyboardEvent) => void;
  sideOffset?: number;
  delayDuration?: number;
  disableHoverableContent?: boolean;
  className?: string;
}

export function Tooltip({
  align,
  arrow = false,
  arrowClassName,
  children,
  className,
  content,
  contentClassName,
  delayDuration,
  disableHoverableContent,
  interactiveChild,
  onClick,
  onKeyDown,
  side,
  sideOffset,
  ...props
}: TooltipProps) {
  return (
    <TooltipPrimitive.Root
      delayDuration={delayDuration}
      disableHoverableContent={disableHoverableContent}
      {...props}
    >
      <TooltipPrimitive.Trigger
        asChild={interactiveChild}
        onClick={onClick}
        onKeyDown={onKeyDown}
        className={className}
      >
        {children}
      </TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content
          className={cn(
            "z-50 overflow-hidden rounded-md bg-tooltip-foreground px-3 py-1.5 text-xs text-tooltip shadow-md animate-in fade-in-0 zoom-in-95 data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95 data-[side=bottom]:slide-in-from-top-2 data-[side=left]:slide-in-from-right-2 data-[side=right]:slide-in-from-left-2 data-[side=top]:slide-in-from-bottom-2",
            contentClassName,
          )}
          align={align}
          collisionPadding={8}
          side={side}
          sideOffset={sideOffset ?? 6}
        >
          {arrow && <TooltipPrimitive.Arrow className={arrowClassName} />}
          {content}
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  );
}
