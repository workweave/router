import { Card, CardProps } from "@/components/molecules/Card";
import { Intent } from "@/components/types";
import { cn } from "@/tools/cn";

import { Loader2, TriangleAlert } from "lucide-react";
import React from "react";

export interface ChartMessageCardProps extends CardProps {
  /**
   * If true, renders an overlay over the chart below.
   *
   * @default false
   */
  overlay?: boolean;
}

export function ChartMessageCard({ className, overlay = false, ...props }: ChartMessageCardProps) {
  return (
    <>
      {overlay && (
        <div className="pointer-events-none absolute inset-0 z-10 animate-fade-in">
          <div className="h-full w-full bg-background opacity-60" />
        </div>
      )}

      <Card
        className={cn("absolute-center z-10 max-h-full min-w-[60%] overflow-auto", className)}
        size="sm"
        {...props}
      />
    </>
  );
}

interface ChartMessageCardEmptyProps {
  emptyMessage?: React.ReactNode;
}

ChartMessageCard.Empty = function ChartMessageCardEmpty({
  emptyMessage,
}: ChartMessageCardEmptyProps) {
  return (
    <ChartMessageCard>
      {React.Children.count(emptyMessage) > 0 ?
        emptyMessage
      : <Card.Header className="text-center">
          <Card.Title>No data yet</Card.Title>
        </Card.Header>
      }
    </ChartMessageCard>
  );
};

ChartMessageCard.Loading = function ChartMessageCardLoading() {
  return (
    <ChartMessageCard className="w-min min-w-0 rounded-full p-2" overlay>
      <Card.Header className="text-center">
        <Loader2 className="size-6 animate-spin" />
      </Card.Header>
    </ChartMessageCard>
  );
};

interface ChartMessageCardErrorProps {
  error: Error;
}

ChartMessageCard.Error = function ChartMessageCardError({ error }: ChartMessageCardErrorProps) {
  return (
    <ChartMessageCard
      icon={<TriangleAlert className="size-5 text-danger" />}
      intent={Intent.Danger}
    >
      <Card.Header>
        <Card.Title>Could not load data</Card.Title>
        <Card.Description>
          Error: {error instanceof Error ? error.message : String(error)}
        </Card.Description>
      </Card.Header>
    </ChartMessageCard>
  );
};
