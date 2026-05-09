"use client";

import { cn } from "@/lib/cn";
import { Command as CommandPrimitive } from "cmdk";
import { Loader2, Search } from "lucide-react";
import React, { forwardRef, useCallback, useEffect, useRef } from "react";

type CommandType = typeof CommandPrimitive & {
  Empty: typeof CommandEmpty;
  Group: typeof CommandGroup;
  Input: typeof CommandInput;
  Item: typeof CommandItem;
  List: typeof CommandList;
  Loading: typeof CommandLoading;
  Separator: typeof CommandSeparator;
  Shortcut: typeof CommandShortcut;
};

export const Command = forwardRef<
  React.ElementRef<typeof CommandPrimitive>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive>
>(({ className, ...props }, ref) => (
  <CommandPrimitive
    ref={ref}
    loop
    className={cn("flex h-full w-full flex-col text-popover-foreground", className)}
    {...props}
  />
)) as CommandType;
Command.displayName = CommandPrimitive.displayName;

interface CommandInputProps
  extends React.ComponentPropsWithoutRef<typeof CommandPrimitive.Input> {
  hideIcon?: boolean;
  containerClassName?: string;
}

const CommandInput = forwardRef<
  React.ElementRef<typeof CommandPrimitive.Input>,
  CommandInputProps
>(({ children, className, containerClassName, hideIcon = false, ...props }, ref) => (
  <div
    className={cn("flex items-center gap-2 border-b px-3", containerClassName)}
    cmdk-input-wrapper=""
  >
    {!hideIcon && <Search className="size-4 shrink-0 text-muted-foreground" />}
    {children}
    <CommandPrimitive.Input
      ref={ref}
      className={cn(
        "flex h-11 w-full rounded-md bg-background py-3 text-sm outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...props}
    />
  </div>
));
CommandInput.displayName = CommandPrimitive.Input.displayName;

const CommandList = forwardRef<
  React.ElementRef<typeof CommandPrimitive.List>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.List>
>(({ className, ...props }, ref) => {
  const internalRef = useRef<HTMLDivElement | null>(null);

  // cmdk reorders DOM during filter; nudge the selected item back into view.
  useEffect(() => {
    const el = internalRef.current;
    if (el == null) return;
    let rafId: number | undefined;
    const observer = new MutationObserver(() => {
      if (rafId !== undefined) cancelAnimationFrame(rafId);
      rafId = requestAnimationFrame(() => {
        rafId = undefined;
        const selected = el.querySelector<HTMLElement>('[aria-selected="true"]');
        if (selected != null) selected.scrollIntoView({ block: "nearest" });
      });
    });
    observer.observe(el, { attributeFilter: ["aria-selected"], attributes: true, subtree: true });
    return () => {
      observer.disconnect();
      if (rafId !== undefined) cancelAnimationFrame(rafId);
    };
  }, []);

  const setRef = useCallback(
    (node: HTMLDivElement | null) => {
      internalRef.current = node;
      if (typeof ref === "function") ref(node);
      else if (ref != null) ref.current = node;
    },
    [ref],
  );

  return (
    <CommandPrimitive.List
      ref={setRef}
      className={cn("max-h-96 scroll-p-4 overflow-y-auto overflow-x-hidden", className)}
      {...props}
    />
  );
});
CommandList.displayName = CommandPrimitive.List.displayName;

const CommandEmpty = forwardRef<
  React.ElementRef<typeof CommandPrimitive.Empty>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Empty>
>((props, ref) => (
  <CommandPrimitive.Empty
    ref={ref}
    className="py-6 text-center text-sm text-muted-foreground"
    {...props}
  />
));
CommandEmpty.displayName = CommandPrimitive.Empty.displayName;

const CommandGroup = forwardRef<
  React.ElementRef<typeof CommandPrimitive.Group>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Group>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Group
    ref={ref}
    className={cn(
      "overflow-hidden p-1 text-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:font-medium [&_[cmdk-group-heading]]:text-muted-foreground",
      className,
    )}
    {...props}
  />
));
CommandGroup.displayName = CommandPrimitive.Group.displayName;

const CommandSeparator = forwardRef<
  React.ElementRef<typeof CommandPrimitive.Separator>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Separator>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Separator
    ref={ref}
    alwaysRender
    className={cn("h-px bg-border first:hidden last:hidden", className)}
    {...props}
  />
));
CommandSeparator.displayName = CommandPrimitive.Separator.displayName;

const CommandItem = forwardRef<
  React.ElementRef<typeof CommandPrimitive.Item>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Item>
>(({ className, ...props }, ref) => (
  <CommandPrimitive.Item
    ref={ref}
    className={cn(
      "relative flex min-w-0 cursor-pointer select-none items-center gap-2 rounded p-2 text-sm outline-none aria-selected:bg-accent aria-selected:text-accent-foreground data-[disabled=true]:pointer-events-none data-[disabled=true]:opacity-50",
      className,
    )}
    {...props}
  />
));
CommandItem.displayName = CommandPrimitive.Item.displayName;

const CommandShortcut = ({ className, ...props }: React.HTMLAttributes<HTMLSpanElement>) => (
  <span className={cn("ml-auto text-xs tracking-widest text-muted-foreground", className)} {...props} />
);
CommandShortcut.displayName = "CommandShortcut";

const CommandLoading = forwardRef<
  React.ElementRef<typeof CommandPrimitive.Loading>,
  React.ComponentPropsWithoutRef<typeof CommandPrimitive.Loading>
>(({ children, className, ...props }, ref) => (
  <CommandGroup>
    <CommandPrimitive.Loading
      ref={ref}
      className={cn(
        "relative cursor-default select-none items-center gap-2 rounded p-2 text-sm text-muted-foreground",
        className,
      )}
      {...props}
    >
      <div className="flex flex-row items-center gap-2">
        <Loader2 className="size-4 animate-spin" />
        {children}
      </div>
    </CommandPrimitive.Loading>
  </CommandGroup>
));
CommandLoading.displayName = CommandPrimitive.Loading.displayName;

Command.Empty = CommandEmpty;
Command.Group = CommandGroup;
Command.Input = CommandInput;
Command.Item = CommandItem;
Command.List = CommandList;
Command.Loading = CommandLoading;
Command.Separator = CommandSeparator;
Command.Shortcut = CommandShortcut;
