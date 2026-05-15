import { cn } from "@/lib/cn";
import * as PopoverPrimitive from "@radix-ui/react-popover";
import React from "react";

type PopoverType = typeof PopoverPrimitive.Root & {
  Content: typeof PopoverContent;
  Trigger: typeof PopoverTrigger;
};

export const Popover: PopoverType = PopoverPrimitive.Root as PopoverType;

const PopoverTrigger = React.forwardRef<
  React.ElementRef<typeof PopoverPrimitive.Trigger>,
  Omit<React.ComponentPropsWithoutRef<typeof PopoverPrimitive.Trigger>, "asChild">
>(({ className, ...props }, ref) => (
  <PopoverPrimitive.Trigger
    ref={ref}
    className={cn("cursor-pointer", className)}
    asChild
    {...props}
  />
));
PopoverTrigger.displayName = PopoverPrimitive.Trigger.displayName;

const PopoverContent = React.forwardRef<
  React.ElementRef<typeof PopoverPrimitive.Content>,
  React.ComponentPropsWithoutRef<typeof PopoverPrimitive.Content>
>(({ align = "center", className, sideOffset = 6, ...props }, ref) => (
  <PopoverPrimitive.Portal>
    <PopoverPrimitive.Content
      ref={ref}
      align={align}
      sideOffset={sideOffset}
      collisionPadding={8}
      className={cn(
        "z-50 w-72 rounded-lg border bg-popover p-4 text-popover-foreground shadow-md outline-none data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[side=bottom]:slide-in-from-top-2 data-[side=bottom]:slide-out-to-top-1 data-[side=left]:slide-in-from-right-2 data-[side=left]:slide-out-to-right-1 data-[side=right]:slide-in-from-left-2 data-[side=right]:slide-out-to-left-1 data-[side=top]:slide-in-from-bottom-2 data-[side=top]:slide-out-to-bottom-1",
        className,
      )}
      {...props}
    />
  </PopoverPrimitive.Portal>
));
PopoverContent.displayName = PopoverPrimitive.Content.displayName;

Popover.Trigger = PopoverTrigger;
Popover.Content = PopoverContent;
