import { cn } from "@/tools/cn";

export interface SkeletonProps extends React.ComponentProps<"div"> {
  /**
   * The element to render.
   *
   * @default "div"
   */
  as?: "div" | "span";
  /**
   * Whether to use a darker variant.
   *
   * @default false
   */
  darker?: boolean;
}

/**
 * Renders a loading skeleton.
 */
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

/**
 * Renders a skeleton exactly matching the size and spacing of text.
 */
Skeleton.Text = function SkeletonText({
  as: Element = "div",
  children,
  className,
  darker = false,
  size = "base",
  ...props
}: SkeletonProps & {
  size?: "2xl" | "4xl" | "base" | "h1" | "h2" | "h3" | "h4" | "lg" | "p" | "sm" | "xl" | "xs";
}) {
  return (
    <Element
      className={cn(
        {
          "h-[1.5rem] py-[0.25rem]": size === "base" || size === "p" || size === "h4",
          "h-[1.25rem] py-[0.1875rem]": size === "sm",
          "h-[1.75rem] py-[0.25rem]": size === "xl" || size === "h3",
          "h-[1.75rem] py-[0.3125rem]": size === "lg",
          "h-[1rem] py-[0.09375rem]": size === "xs",
          "h-[2.5rem] py-[0.125rem]": size === "4xl" || size === "h1",
          "h-[2rem] py-[0.25rem]": size === "h2" || size === "2xl",
        },
        className,
      )}
      {...props}
    >
      <Skeleton className="h-full w-full" as={Element} darker={darker}>
        {children}
      </Skeleton>
    </Element>
  );
};
