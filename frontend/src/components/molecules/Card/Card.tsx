import { Skeleton } from "@/components/atoms/Skeleton";
import { Text, TextProps } from "@/components/atoms/Text";
import { Intent } from "@/components/types";
import { cn } from "@/lib/cn";
import { cva, VariantProps } from "class-variance-authority";
import React from "react";

const cardVariants = cva(
  "group flex flex-col gap-x-4 rounded-lg border bg-card text-card-foreground shadow-sm",
  {
    defaultVariants: {
      intent: Intent.Default,
      size: "default",
    },
    variants: {
      intent: {
        [Intent.Danger]: "border-danger/25",
        [Intent.Default]: "",
        [Intent.Primary]: "border-primary/25",
        [Intent.Success]: "border-success/25",
        [Intent.Warning]: "border-warning/25",
      },
      size: {
        default: "gap-y-6 p-6",
        sm: "card-small gap-y-4 p-4",
      },
    },
  },
);

export interface CardProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof cardVariants> {
  icon?: React.ReactNode;
}

type CardType = React.ForwardRefExoticComponent<CardProps & React.RefAttributes<HTMLDivElement>> & {
  Content: typeof CardContent;
  Description: typeof CardDescription;
  Footer: typeof CardFooter;
  Header: typeof CardHeader;
  Loading: typeof CardLoading;
  Title: typeof CardTitle;
};

export const Card: CardType = React.forwardRef<HTMLDivElement, CardProps>(
  ({ children, className, icon, intent, size, ...props }, ref) => {
    const hasIcon = icon != null;
    const small = size === "sm";
    return (
      <div
        ref={ref}
        className={cn(cardVariants({ intent, size }), hasIcon ? "flex-row" : "flex-col", className)}
        {...props}
      >
        {hasIcon ? (
          <>
            <div className="flex shrink-0 grow-0">{icon}</div>
            <div className={cn("flex min-w-0 grow flex-col", small ? "gap-y-4" : "gap-y-6")}>
              {children}
            </div>
          </>
        ) : (
          children
        )}
      </div>
    );
  },
) as CardType;
Card.displayName = "Card";

const CardHeader = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => (
    <div ref={ref} className={cn("flex flex-col gap-2", className)} {...props} />
  ),
);
CardHeader.displayName = "CardHeader";

export interface CardTitleProps extends React.HTMLAttributes<HTMLHeadingElement> {
  variant?: TextProps["variant"];
}

function CardTitle({ className, variant, ...props }: CardTitleProps) {
  return (
    <Text
      variant={variant ?? "h3"}
      className={cn(
        "leading-none",
        variant == null &&
          "group-[.card-small]:text-base group-[.card-small]:font-medium group-[.card-small]:leading-tight",
        className,
      )}
      {...props}
    />
  );
}

function CardDescription({ className, ...props }: React.HTMLAttributes<HTMLParagraphElement>) {
  return <Text className={cn("text-sm text-muted-foreground", className)} {...props} />;
}

const CardContent = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => <div ref={ref} className={className} {...props} />,
);
CardContent.displayName = "CardContent";

const CardFooter = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => (
    <div ref={ref} className={cn("flex items-center p-6 pt-0", className)} {...props} />
  ),
);
CardFooter.displayName = "CardFooter";

const CardLoading = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  (props, ref) => {
    return (
      <Card ref={ref} {...props}>
        <Card.Header>
          <Card.Title>
            <Skeleton className="h-7 w-1/2 max-w-24" />
          </Card.Title>
          <Card.Description>
            <Skeleton className="h-3 w-3/4 max-w-36" as="span" />
          </Card.Description>
        </Card.Header>
        <Card.Content className="flex flex-col gap-1">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-5/6" />
        </Card.Content>
      </Card>
    );
  },
);
CardLoading.displayName = "CardLoading";

Card.Header = CardHeader;
Card.Title = CardTitle;
Card.Description = CardDescription;
Card.Content = CardContent;
Card.Footer = CardFooter;
Card.Loading = CardLoading;
