import { Text } from "@/components/atoms/Text";
import { cn } from "@/tools/cn";
import { LoadState } from "@/tools/LoadState";

export interface StatisticProps {
  className?: string;
  statistic: LoadState<{ total?: string }>;
}

export function Statistic({ className, statistic }: StatisticProps) {
  const value = LoadState.unwrap(statistic);
  const isLoading = LoadState.isLoading(statistic);

  return (
    <div className={cn("flex flex-col items-end", className)}>
      {isLoading ?
        <div className="h-9 w-24 animate-pulse rounded bg-muted" />
      : <Text
          variant="h1"
          as="p"
          className="whitespace-nowrap text-xl font-semibold text-foreground @md:text-2xl @2xl:text-4xl"
        >
          {value?.total ?? "-"}
        </Text>
      }
    </div>
  );
}
