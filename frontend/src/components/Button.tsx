import Link from "next/link";
import { type ButtonHTMLAttributes, forwardRef } from "react";

import { cn } from "@/lib/cn";

type Appearance = "filled" | "outline" | "hollow" | "ghost" | "danger";
type Size = "sm" | "md" | "icon";

interface BaseProps {
  variant?: Appearance;
  size?: Size;
  href?: string;
}

type ButtonProps = BaseProps & ButtonHTMLAttributes<HTMLButtonElement>;

const BASE =
  "inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 focus-visible:ring-offset-background disabled:pointer-events-none disabled:opacity-50";

const VARIANTS: Record<Appearance, string> = {
  filled: "bg-foreground text-background hover:bg-foreground/90",
  outline:
    "border border-border-darker bg-background text-foreground hover:bg-accent hover:text-accent-foreground",
  hollow:
    "border border-transparent text-foreground hover:bg-accent hover:text-accent-foreground aria-selected:border-border-darker aria-selected:bg-muted",
  ghost: "text-foreground hover:bg-accent hover:text-accent-foreground",
  danger: "bg-danger text-danger-foreground hover:bg-danger/90",
};

const SIZES: Record<Size, string> = {
  sm: "h-8 px-3 text-xs",
  md: "h-9 px-4 text-xs",
  icon: "size-8",
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  ({ variant = "outline", size = "md", className, href, type, children, ...props }, ref) => {
    const classes = cn(BASE, VARIANTS[variant], SIZES[size], className);

    if (href) {
      return (
        <Link href={href} className={classes}>
          {children}
        </Link>
      );
    }

    return (
      <button ref={ref} type={type ?? "button"} className={classes} {...props}>
        {children}
      </button>
    );
  },
);
Button.displayName = "Button";

export type { ButtonProps };
