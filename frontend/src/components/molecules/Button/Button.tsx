import { Skeleton } from "@/components/atoms/Skeleton";
import { Appearance, Intent } from "@/components/types";
import { cn } from "@/lib/cn";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import Link from "next/link";
import React, { forwardRef } from "react";

const buttonVariants = cva(
  "button border-[hsl(var(--button-text-color)/.25)] bg-[hsl(var(--button-bg-color))] text-[hsl(var(--button-text-color))] hover:bg-[hsl(var(--button-hover-bg-color)/.05)] active:bg-[hsl(var(--button-hover-bg-color)/.1)] aria-expanded:bg-[hsl(var(--button-hover-bg-color)/.05)] aria-expanded:hover:bg-[hsl(var(--button-hover-bg-color)/.1)] aria-expanded:active:bg-[hsl(var(--button-hover-bg-color)/.15)] aria-selected:bg-[hsl(var(--button-hover-bg-color)/.05)] aria-selected:hover:bg-[hsl(var(--button-hover-bg-color)/.1)] aria-selected:active:bg-[hsl(var(--button-hover-bg-color)/.15)]",
  {
    defaultVariants: {
      appearance: Appearance.Outlined,
      intent: Intent.Default,
      size: "default",
    },
    variants: {
      intent: {
        [Intent.Danger]: "focus-visible:ring-danger",
        [Intent.Default]: "focus-visible:ring-foreground",
        [Intent.Primary]: "focus-visible:ring-primary",
        [Intent.Success]: "focus-visible:ring-success",
        [Intent.Warning]: "focus-visible:ring-warning",
      },
      appearance: {
        [Appearance.Filled]:
          "hover:bg-[hsl(var(--button-hover-bg-color)/.9)] active:bg-[hsl(var(--button-hover-bg-color)/.8)] aria-expanded:bg-[hsl(var(--button-hover-bg-color)/.9)] aria-expanded:active:bg-[hsl(var(--button-hover-bg-color)/.8)]",
        [Appearance.Hollow]: "",
        [Appearance.Outlined]: "border",
      },
      size: {
        default: "h-10 px-4 py-2",
        icon: "size-8",
        lg: "h-11 rounded-md px-8",
        sm: "h-8 rounded-md px-3",
      },
    },
  },
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  href?: React.ComponentProps<typeof Link>["href"];
  prefetch?: boolean;
  newTab?: boolean;
}

type ButtonType = React.ForwardRefExoticComponent<
  React.PropsWithoutRef<ButtonProps> & React.RefAttributes<HTMLButtonElement>
> & {
  Loading: React.ComponentType<LoadingButtonProps>;
};

const BUTTON_COLORS: Record<Appearance, Record<Intent, React.CSSProperties>> = {
  [Appearance.Filled]: {
    [Intent.Danger]: {
      "--button-bg-color": "var(--danger)",
      "--button-hover-bg-color": "var(--button-bg-color)",
      "--button-text-color": "var(--danger-foreground)",
    } as React.CSSProperties,
    [Intent.Default]: {
      "--button-bg-color": "var(--foreground)",
      "--button-hover-bg-color": "var(--button-bg-color)",
      "--button-text-color": "var(--background)",
    } as React.CSSProperties,
    [Intent.Primary]: {
      "--button-bg-color": "var(--primary)",
      "--button-hover-bg-color": "var(--button-bg-color)",
      "--button-text-color": "var(--primary-foreground)",
    } as React.CSSProperties,
    [Intent.Success]: {
      "--button-bg-color": "var(--success)",
      "--button-hover-bg-color": "var(--button-bg-color)",
      "--button-text-color": "var(--success-foreground)",
    } as React.CSSProperties,
    [Intent.Warning]: {
      "--button-bg-color": "var(--warning)",
      "--button-hover-bg-color": "var(--button-bg-color)",
      "--button-text-color": "var(--warning-foreground)",
    } as React.CSSProperties,
  },
  [Appearance.Hollow]: {
    [Intent.Danger]: {
      "--button-hover-bg-color": "var(--danger)",
      "--button-text-color": "var(--danger)",
    } as React.CSSProperties,
    [Intent.Default]: {
      "--button-hover-bg-color": "var(--foreground)",
      "--button-text-color": "var(--foreground)",
    } as React.CSSProperties,
    [Intent.Primary]: {
      "--button-hover-bg-color": "var(--primary)",
      "--button-text-color": "var(--primary)",
    } as React.CSSProperties,
    [Intent.Success]: {
      "--button-hover-bg-color": "var(--success)",
      "--button-text-color": "var(--success)",
    } as React.CSSProperties,
    [Intent.Warning]: {
      "--button-hover-bg-color": "var(--warning)",
      "--button-text-color": "var(--warning)",
    } as React.CSSProperties,
  },
  [Appearance.Outlined]: {
    [Intent.Danger]: {
      "--button-hover-bg-color": "var(--danger)",
      "--button-text-color": "var(--danger)",
    } as React.CSSProperties,
    [Intent.Default]: {
      "--button-hover-bg-color": "var(--foreground)",
      "--button-text-color": "var(--foreground)",
    } as React.CSSProperties,
    [Intent.Primary]: {
      "--button-hover-bg-color": "var(--primary)",
      "--button-text-color": "var(--primary)",
    } as React.CSSProperties,
    [Intent.Success]: {
      "--button-hover-bg-color": "var(--success)",
      "--button-text-color": "var(--success)",
    } as React.CSSProperties,
    [Intent.Warning]: {
      "--button-hover-bg-color": "var(--warning)",
      "--button-text-color": "var(--warning)",
    } as React.CSSProperties,
  },
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  (
    {
      appearance,
      children,
      className,
      disabled,
      href,
      intent,
      newTab = false,
      prefetch,
      size,
      style,
      ...buttonProps
    },
    ref,
  ) => {
    const isLink = href != null && disabled !== true;
    const isApiRoute = typeof href === "string" && href.startsWith("/api");

    const buttonChildren = isLink ? (
      <Link
        href={href}
        prefetch={prefetch ?? !isApiRoute}
        {...(newTab ? { rel: "noopener noreferrer", target: "_blank" } : {})}
      >
        {children}
      </Link>
    ) : (
      children
    );

    const As = isLink ? Slot : "button";

    return (
      <As
        {...buttonProps}
        disabled={disabled}
        ref={ref}
        className={cn(buttonVariants({ appearance, intent, size }), className)}
        style={{
          ...style,
          ...BUTTON_COLORS[appearance ?? Appearance.Outlined][intent ?? Intent.Default],
        }}
      >
        {buttonChildren}
      </As>
    );
  },
) as ButtonType;
Button.displayName = "Button";

interface LoadingButtonProps extends Pick<ButtonProps, "className" | "size"> {}

Button.Loading = function LoadingButton({ className, size }: LoadingButtonProps) {
  return (
    <Skeleton
      className={cn(
        buttonVariants({ appearance: Appearance.Hollow, size }),
        {
          "w-20": size === "sm",
          "w-24": size === "default",
          "w-32": size === "lg",
        },
        "block",
        className,
      )}
      style={{ "--button-bg-color": "var(--muted)" } as React.CSSProperties}
    />
  );
};
